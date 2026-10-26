// Warmteregelaar: stuurt boiler- en kamertemperatuur van de Panasonic-warmtepomp (via de HeishaMon)
// met tijdvensters, de prijzen en zonneprognose van evcc, en wat hij zelf leert over boiler en huis.
//
// Start (Synology, Task Scheduler):  ACT=0 ./warmteregelaar     (ACT=0 = proef: alleen loggen)
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // tijdzone ingebakken: werkt ook op de Synology
)

var (
	hmURL   = env("HM", "http://192.168.0.254")
	evccURL = env("EVCC", "http://127.0.0.1:7070")
	listen  = env("LISTEN", ":7080")
	dataDir = env("DATA", ".")
	act     = env("ACT", "0") == "1"
	tick    = time.Duration(envInt("INTERVAL", 60)) * time.Second
	client  = &http.Client{Timeout: 8 * time.Second}
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func envInt(k string, d int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return d
}
func envFloat(k string, d float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return d
}

// ---------- toestand ----------

type State struct {
	Learned  Learned   `json:"learned"`
	Schedule *Schedule `json:"schedule,omitempty"` // weekschema (leeg = standaardschema)
	PVBoost  bool      `json:"pvBoost"`
	// zomerstand: de regelaar zette de warmtepomp op "alleen sanitair water"; daarna terug naar SummerPrev
	SummerOn    bool      `json:"summerOn"`
	SummerPrev  float64   `json:"summerPrev"`
	SummerAt    time.Time `json:"summerAt"`
	ModeFixAt   time.Time `json:"modeFixAt"`   // wanneer de regelaar sanitair water weer bij de stand zette
	DHWAskSince time.Time `json:"dhwAskSince"` // sinds wanneer de boiler opwarmen moet zonder dat de warmtepomp het doet
	// laatste doelen die de regelaar stuurde (om manuele aanpassingen te herkennen)
	SentDHW  float64   `json:"sentDHW"`
	SentZ1   float64   `json:"sentZ1"`
	PrevDHW  float64   `json:"prevDHW"`
	PrevZ1   float64   `json:"prevZ1"`   // vorig doel van de regelaar voor de kamer
	Override time.Time `json:"override"` // tot wanneer een manuele aanpassing van de kamertemperatuur voorgaat
	// manuele aanpassing van de boiler (bv. voor een bad): geldt tot de boiler warm is, hoogstens 3 uur
	DHWOverride time.Time `json:"dhwOverride"`
	DHWManual   float64   `json:"dhwManual"`
	SeenDHW     float64   `json:"seenDHW"` // vorige waarneming (een aanpassing telt pas als ze blijft staan)
	SeenZ1      float64   `json:"seenZ1"`
	// wanneer de regelaar zelf iets stuurde: de HeishaMon toont dat pas na ±10–60 s
	SentDHWAt time.Time `json:"sentDHWAt"`
	SentZ1At  time.Time `json:"sentZ1At"`
	// lopende metingen om bij te leren
	Preheat      *preheatRun `json:"preheat,omitempty"`
	DHWMark      *mark       `json:"dhwMark,omitempty"`
	DHWQ         *dhwQuarter `json:"dhwQ,omitempty"` // lopend kwartier: om het verbruik per kwartier te leren
	PVHotOn      bool        `json:"pvHotOn"`        // zonne-overschot tot 60 °C: blijft tot het overschot stopt
	PlanLogged   string      `json:"planLogged"`     // laatst gelogde opwarmblokken
	PlanLoggedAt time.Time   `json:"planLoggedAt"`
	Current      Forecast    `json:"-"`         // voorspelling van deze ronde (voor de grafieken)
	Forecasts    []Forecast  `json:"forecasts"` // elk uur bewaard: om later te vergelijken met wat er gebeurde
	RoomMark     *roomMark   `json:"roomMark,omitempty"`
	LastLog      time.Time   `json:"lastLog"`
	// laadplan voor de auto (zie laden.go)
	ChargePlan    ChargePlan `json:"chargePlan"`
	ChargePlanFor time.Time  `json:"chargePlanFor"` // vertrektijd waarvoor het plan al gezet is
	CarFull       bool       `json:"carFull"`       // auto zeker vol (zie carFull)
	CarFullAt     time.Time  `json:"carFullAt"`
	CarIdleSince  time.Time  `json:"carIdleSince"`
	// legionella (zie legionella.go)
	LegioLast  time.Time `json:"legioLast"` // laatste keer ≥ 59 °C
	LegioOn    bool      `json:"legioOn"`
	LegioStart time.Time `json:"legioStart"`
	LegioFail  time.Time `json:"legioFail"`
	LegioWhy   string    `json:"legioWhy"`
	// Shelly-thermometer: verschil met de sensor van de warmtepomp (zie shelly.go)
	RoomOffset  float64 `json:"roomOffset"`
	RoomOffsetN int     `json:"roomOffsetN"`
	Status      Status  `json:"-"`
}

type preheatRun struct {
	Start  time.Time `json:"start"`
	Tin    float64   `json:"tin"`
	Target float64   `json:"target"`
	ToutS  float64   `json:"toutSum"`
	N      int       `json:"n"`
}

type dhwQuarter struct {
	Start  time.Time `json:"start"`
	Temp   float64   `json:"temp"`
	Heated bool      `json:"heated"`
}

type mark struct {
	Time    time.Time `json:"time"`
	Temp    float64   `json:"temp"`
	Heating bool      `json:"heating"`
}

type Status struct {
	Time time.Time `json:"time"`
	Act  bool      `json:"act"`
	Mode string    `json:"mode"`
	DHW  struct {
		Temp, Target, Want float64
		Why                string
		Override           time.Time
		Next               time.Time // volgende geplande opwarming
		ForecastStart      time.Time
		Forecast           []float64 // voorspelde temperatuur per kwartier vanaf ForecastStart
	} `json:"dhw"`
	Heat struct {
		Tin, Tout, Set, Want float64
		Why                  string
		NextWindow           time.Time
		PreheatStart         time.Time
		LeadMinutes          float64
		Override             time.Time
		HPTin                float64     // sensor van de warmtepomp (hele graden)
		Shelly               *shellyData // wifi-thermometer (nil = geen verse meting)
		Offset               float64     // geleerd verschil warmtepomp − Shelly
	} `json:"heat"`
	Legionella legioStatus `json:"legionella"`
	Learned    Learned     `json:"learned"`
	Battery    battState   `json:"battery"`
	Error      string      `json:"error,omitempty"`
}

var (
	mu    sync.Mutex
	st    State
	cfg   = defaultConfig()
	sched Schedule // standaardschema (uit de omgevingsvariabelen), tot er een in de toestand staat
)

// huidig weekschema
func schedule() Schedule {
	if st.Schedule != nil {
		return *st.Schedule
	}
	return sched
}

func statePath() string { return filepath.Join(dataDir, "warmteregelaar-state.json") }

func loadState() {
	st.Learned = defaultLearned()
	st.ChargePlan = defaultChargePlan()
	if b, err := os.ReadFile(statePath()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.ChargePlan.KWh == 0 {
		st.ChargePlan = defaultChargePlan()
	}
}
func saveState() {
	b, _ := json.MarshalIndent(st, "", "  ")
	_ = os.WriteFile(statePath()+".tmp", b, 0o644)
	_ = os.Rename(statePath()+".tmp", statePath())
}

// ---------- HeishaMon en evcc ----------

type hmData map[string]float64

// De regelaar is de enige die de HeishaMon uitleest (één keer per HMPOLL seconden, nooit twee vragen
// tegelijk). evcc, de Stroom-app en de piekbewaker krijgen een kopie via /json; commando's gaan via /command.
// Zo krijgt het kleine wifi-bordje veel minder vragen.
var (
	hmPoll  = time.Duration(envInt("HMPOLL", 10)) * time.Second
	hmReqMu sync.Mutex // één vraag tegelijk naar de HeishaMon
	hmMu    sync.Mutex
	hmBody  []byte
	hmTime  time.Time
	hmErr   error
)

func hmGet(path string) ([]byte, error) {
	hmReqMu.Lock()
	defer hmReqMu.Unlock()
	resp, err := client.Get(hmURL + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HeishaMon: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func pollHM() {
	for {
		b, err := hmGet("/json")
		hmMu.Lock()
		if err == nil && json.Valid(b) {
			hmBody, hmTime, hmErr = b, time.Now(), nil
		} else if err != nil {
			hmErr = err
		}
		hmMu.Unlock()
		time.Sleep(hmPoll)
	}
}

// laatste HeishaMon-gegevens, als ze niet ouder zijn dan maxAge
func hmCached(maxAge time.Duration) ([]byte, error) {
	hmMu.Lock()
	defer hmMu.Unlock()
	if hmBody == nil || time.Since(hmTime) > maxAge {
		if hmErr != nil {
			return nil, hmErr
		}
		return nil, fmt.Errorf("nog geen gegevens van de HeishaMon")
	}
	return hmBody, nil
}

func readHM() (hmData, error) {
	body, err := hmCached(2 * time.Minute)
	if err != nil {
		return nil, err
	}
	var d struct {
		Heatpump []struct {
			Name  string `json:"Name"`
			Value any    `json:"Value"`
		} `json:"heatpump"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	res := hmData{}
	for _, x := range d.Heatpump {
		switch v := x.Value.(type) {
		case float64:
			res[x.Name] = v
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				res[x.Name] = f
			}
		}
	}
	if _, ok := res["DHW_Temp"]; !ok {
		return nil, fmt.Errorf("geen DHW_Temp in HeishaMon-gegevens")
	}
	return res, nil
}

func sendHM(cmd string, v float64) error {
	u := fmt.Sprintf("%s/command?%s=%s", hmURL, cmd, strconv.Itoa(int(math.Round(v))))
	if !act {
		return errProef
	}
	if _, err := hmGet(u[len(hmURL):]); err != nil {
		log.Printf("⚠ HeishaMon %s=%.0f niet gelukt: %v", cmd, v, err)
		return err
	}
	log.Printf("→ HeishaMon %s=%.0f", cmd, v)
	return nil
}

var errProef = fmt.Errorf("proef: niets verstuurd")

// logChange: alleen loggen als de beslissing verandert (geen herhaling elke minuut)
var lastMsg = map[string]string{}

func logChange(key, msg string) {
	if lastMsg[key] != msg {
		lastMsg[key] = msg
		if !act {
			msg = "(proef, niets verstuurd) " + msg
		}
		log.Print(msg)
	}
}

var rateCache = map[string]struct {
	t     time.Time
	rates []Rate
}{}

// evcc /api/tariff/{grid|solar}: {"rates":[…]} (of {"result":{"rates":[…]}} bij oudere versies); 10 min gecachet
func readRates(kind string) []Rate {
	if c, ok := rateCache[kind]; ok && time.Since(c.t) < 10*time.Minute {
		return c.rates
	}
	var rates []Rate
	if resp, err := client.Get(evccURL + "/api/tariff/" + kind); err == nil {
		var d struct {
			Rates  []Rate `json:"rates"`
			Result struct {
				Rates []Rate `json:"rates"`
			} `json:"result"`
		}
		if json.NewDecoder(resp.Body).Decode(&d) == nil {
			rates = d.Rates
			if len(rates) == 0 {
				rates = d.Result.Rates
			}
		}
		resp.Body.Close()
	}
	rateCache[kind] = struct {
		t     time.Time
		rates []Rate
	}{time.Now(), rates}
	return rates
}

// teruglevering nu (W, positief = naar het net) en laadtoestand van de thuisbatterij (%, -1 = onbekend) volgens evcc
func readExport() (float64, float64, bool) {
	resp, err := client.Get(evccURL + "/api/state?jq=" + url.QueryEscape("{g: .grid.power, gp: .gridPower, soc: .battery.soc, bs: .batterySoc}"))
	if err != nil {
		return 0, -1, false
	}
	defer resp.Body.Close()
	var d map[string]any
	if json.NewDecoder(resp.Body).Decode(&d) != nil {
		return 0, -1, false
	}
	if r, ok := d["result"].(map[string]any); ok {
		d = r
	}
	num := func(k string) (float64, bool) { v, ok := d[k].(float64); return v, ok }
	soc := -1.0
	if v, ok := num("soc"); ok {
		soc = v
	} else if v, ok := num("bs"); ok {
		soc = v
	}
	for _, k := range []string{"g", "gp"} {
		if v, ok := num(k); ok {
			return -v, soc, true
		}
	}
	return 0, soc, false
}

// ---------- één ronde ----------

func step(now time.Time) {
	mu.Lock()
	defer mu.Unlock()
	s := &st.Status
	sc := schedule()
	s.Time, s.Act, s.Mode, s.Error = now, act, sc.Mode, ""
	hm, err := readHM()
	if err != nil {
		s.Error = "HeishaMon: " + err.Error()
		log.Print(s.Error)
		return
	}
	prices, solar, feedin := readRates("grid"), readRates("solar"), readRates("feedin")
	dhw, dhwSet := hm["DHW_Temp"], hm["DHW_Target_Temp"]
	tin, tout, z1 := hm["Z1_Temp"], hm["Outside_Temp"], hm["Z1_Heat_Request_Temp"]
	dhwHeating := hm["ThreeWay_Valve_State"] == 1 && hm["Compressor_Freq"] > 0
	if tin < -50 { // geen kamersensor: dan de kamerthermostaat
		tin = hm["Room_Thermostat_Temp"]
	}
	// Shelly-thermometer: nauwkeuriger (0,1 °C), op de schaal van de warmtepomp
	s.Heat.HPTin, s.Heat.Shelly = tin, nil
	shTin, shHum := math.NaN(), math.NaN()
	if m, ok := shellyNow(now); ok {
		if tin > -50 {
			st.RoomOffset = roomOffset(st.RoomOffset, st.RoomOffsetN, tin, m.Temp)
			st.RoomOffsetN++
		}
		shTin, shHum = m.Temp, m.Hum
		s.Heat.Shelly, s.Heat.Offset = &m, math.Round(st.RoomOffset*100)/100
		tin = round1(m.Temp + st.RoomOffset)
	}

	learnDHW(now, dhw, dhwHeating)
	learnDHWUse(now, dhw, dhwHeating)

	// boiler
	summer(now, sc, hm)

	// veel zonne-overschot (≥ 3 kW teruglevering naast de boiler) of een negatieve terugleverprijs: tot 60 °C,
	// ook met de elektrische bijverwarming; dat blijft zo tot evcc het overschot stopt.
	// Voorrang: eerst de thuisbatterij (moet ≥ 95 % zijn), dan de auto (evcc), dan pas het sanitair water.
	if !st.PVBoost {
		st.PVHotOn = false
	} else if !st.PVHotOn {
		fp, okf := priceAt(feedin, now)
		if exp, soc, ok := readExport(); soc >= 95 && ((ok && exp >= 3000) || (okf && fp <= 0)) {
			st.PVHotOn = true
		}
	}
	din := DHWInput{Now: now, Temp: dhw, PVBoost: st.PVBoost, PVHot: st.PVHotOn, Prev: st.PrevDHW, Prices: prices, FeedIn: feedin, Solar: solar}
	dp := DHWTarget(cfg, sc, st.Learned, din)
	want, why := dp.Target, dp.Why
	prevPlanned := st.PrevDHW
	st.PrevDHW = dp.Planned
	// geen nieuwe boilerbeurt beginnen terwijl het huis al veel afneemt (kwartierpiek): even uitstellen
	if dp.Planned > 0 && prevPlanned == 0 && !st.PVBoost {
		if exp, _, ok := readExport(); ok && -exp > dhwPeakImport {
			want, why = sc.DHWBase, fmt.Sprintf("opwarmen uitgesteld: het huis neemt nu al %.0f W af (kwartierpiek)", -exp)
			st.PrevDHW = 0
		}
	}
	// legionella: wekelijks ≥ 60 °C (gaat voor op het plan; een manuele aanpassing gaat nog altijd voor)
	if on, lw := legionella(now, dhw, dhwSet, prices, solar); on {
		want, why = legioTemp, lw
	}
	s.DHW.ForecastStart, s.DHW.Forecast, s.DHW.Next = now.Truncate(quarter), make([]float64, len(dp.Forecast)), time.Time{}
	for i, v := range dp.Forecast {
		s.DHW.Forecast[i] = math.Round(v*10) / 10
	}
	if len(dp.Heat) > 0 {
		s.DHW.Next = dp.Heat[0]
	}
	// het plan in het logboek: alleen als de opwarmblokken veranderen, hoogstens om het kwartier
	if b := planBlocks(dp); b != st.PlanLogged && now.Sub(st.PlanLoggedAt) >= quarter {
		st.PlanLogged, st.PlanLoggedAt = b, now
		log.Print("plan sww: " + planSummary(dp))
	}
	// zelf aangepast op de bediening (en blijft staan): voorrang, tot de boiler warm is of hoogstens 3 uur
	if act && st.SentDHW != 0 && now.Sub(st.SentDHWAt) > graceAfterSend && math.Round(dhwSet) == math.Round(st.SeenDHW) && math.Round(dhwSet) != math.Round(st.SentDHW) &&
		math.Round(dhwSet) != math.Round(want) && !now.Before(st.DHWOverride) {
		markDHWOverride(now, dhwSet)
	}
	fresh := !now.Before(st.DHWOverride) && freshChange(dhwSet, st.SeenDHW, st.SentDHW, st.SentDHWAt, now)
	st.SeenDHW = dhwSet
	if now.Before(st.DHWOverride) && st.DHWManual > want && dhw >= st.DHWManual-1 {
		st.DHWOverride = time.Time{} // boiler is warm: terug naar het eigen plan
		log.Printf("boiler heeft de zelf gekozen %.0f °C bereikt: terug naar het plan", st.DHWManual)
	}
	if now.Before(st.DHWOverride) {
		want, why = st.DHWManual, "zelf ingesteld, geldt tot "+st.DHWOverride.Format("15:04")+" of tot de boiler warm is"
	}
	s.DHW.Temp, s.DHW.Target, s.DHW.Want, s.DHW.Why, s.DHW.Override = dhw, dhwSet, want, why, st.DHWOverride
	// warmt de warmtepomp de boiler wel op? (bv. niet als hij op "alleen verwarmen" staat)
	if dhwSet >= dhw+dhwHysteresis && hm["ThreeWay_Valve_State"] != 1 && hm["Heatpump_State"] == 1 {
		if st.DHWAskSince.IsZero() {
			st.DHWAskSince = now
		} else if now.Sub(st.DHWAskSince) > 45*time.Minute && lastMsg["dhwstuck"] == "" {
			// één keer per keer dat het gebeurt (niet elke minuut)
			lastMsg["dhwstuck"] = "gemeld"
			log.Printf("⚠ de boiler staat al %.0f min op %.0f °C (nu %.0f °C) maar de warmtepomp warmt hem niet op (stand %.0f, driewegklep op verwarming)",
				now.Sub(st.DHWAskSince).Minutes(), dhwSet, dhw, hm["Operating_Mode_State"])
		}
	} else {
		st.DHWAskSince = time.Time{}
		lastMsg["dhwstuck"] = ""
	}
	if fresh {
		// net zelf aangepast: eerst kijken of het blijft staan (dan geldt het als eigen keuze), niet overschrijven
	} else if math.Round(dhwSet) != math.Round(want) && (math.Round(st.SentDHW) != math.Round(want) || now.Sub(st.SentDHWAt) > resendAfter) {
		logChange("dhw", fmt.Sprintf("boiler %.0f → %.0f °C: %s", dhwSet, want, why))
		if err := sendHM("SetDHWTemp", want); err == nil {
			st.SentDHW, st.SentDHWAt = want, now
		}
	}

	// verwarming (alleen als de warmtepomp met een kamertemperatuur werkt, niet met een stooklijnverschuiving)
	plan := HeatTarget(cfg, sc, st.Learned, HeatInput{Now: now, Tin: tin, Tout: tout, Prev: st.PrevZ1, Prices: prices})
	st.PrevZ1 = plan.Target
	s.Heat.Tin, s.Heat.Tout, s.Heat.Set, s.Heat.Want, s.Heat.Why = tin, tout, z1, plan.Target, plan.Why
	s.Heat.NextWindow, s.Heat.PreheatStart, s.Heat.LeadMinutes = plan.NextWindow, plan.PreheatStart, plan.LeadMinutes
	if plan.Target == 0 {
		// zomer: verwarming uit, kamertemperatuur niet sturen
	} else if z1 >= 10 {
		// iemand heeft zelf de thermostaat aangepast: respecteren tot het volgende omslagpunt
		if act && st.SentZ1 != 0 && now.Sub(st.SentZ1At) > graceAfterSend && math.Round(z1) == math.Round(st.SeenZ1) && math.Round(z1) != math.Round(st.SentZ1) && math.Round(z1) != math.Round(plan.Target) && now.After(st.Override) {
			st.Override = nextBoundary(now, plan)
			log.Printf("kamertemperatuur zelf op %.0f °C gezet: de regelaar wacht tot %s", z1, st.Override.Format("02/01 15:04"))
		}
		if now.Before(st.Override) {
			s.Heat.Want = z1
			s.Heat.Why = "zelf ingesteld, geldt tot " + st.Override.Format("15:04")
		} else if freshChange(z1, st.SeenZ1, st.SentZ1, st.SentZ1At, now) {
			// net zelf aangepast (bediening): eerst nog een ronde kijken of het blijft staan, niet meteen overschrijven
			s.Heat.Want, s.Heat.Why = z1, "zelf aangepast? even afwachten"
		} else if math.Round(z1) != math.Round(plan.Target) && (math.Round(st.SentZ1) != math.Round(plan.Target) || now.Sub(st.SentZ1At) > resendAfter) {
			logChange("z1", fmt.Sprintf("kamer %.0f → %.0f °C: %s", z1, plan.Target, plan.Why))
			if err := sendHM("SetZ1HeatRequestTemperature", plan.Target); err == nil {
				st.SentZ1, st.SentZ1At = plan.Target, now
			}
		} else if act {
			st.SentZ1 = plan.Target
		}
		st.SeenZ1 = z1
		learnPreheat(now, tin, tout, plan, z1, sc.Eco)
	} else {
		s.Heat.Why = "de warmtepomp werkt met een stooklijnverschuiving: kamertemperatuur niet gestuurd"
	}
	s.Heat.Override = st.Override

	// voorspelling voor de grafieken (en elk uur bewaren)
	room, rset := forecastRoom(cfg, sc, st.Learned, now, tin, tout, plan.Target, prices)
	st.Current = Forecast{Start: dp.Start, Room: room, RoomSet: rset, DHW: s.DHW.Forecast, DHWSet: dhwSetpoints(sc, dp)}
	keepForecast(st.Current)
	if z1 >= 10 {
		learnRoomLoss(now, tin, tout, z1)
	}
	s.Learned = st.Learned

	if now.Sub(st.LastLog) >= 5*time.Minute {
		p, _ := priceAt(prices, now)
		writeCSV(now, hm, tin, want, plan.Target, p, now.Before(st.Override), now.Before(st.DHWOverride), shTin, shHum)
		st.LastLog = now
	}
	saveState()
}

// boven deze afname (W) begint de regelaar geen nieuwe boilerbeurt (kwartierpiek-grens 5 kW)
const dhwPeakImport = 3500

// na een eigen commando zo lang niet besluiten dat iemand anders de temperatuur aanpaste
const graceAfterSend = 2 * time.Minute

// zelfde commando pas opnieuw sturen als de warmtepomp het na zoveel tijd nog niet overnam
const resendAfter = 45 * time.Second

// nieuw schema; bij een andere stand vervallen de manuele aanpassingen (mu moet vast zijn)
func setSchedule(ns Schedule) {
	if old := schedule(); old.Mode != ns.Mode {
		log.Printf("stand: %s → %s", old.Mode, ns.Mode)
		st.Override, st.DHWOverride = time.Time{}, time.Time{}
	} else {
		log.Print("weekschema aangepast")
	}
	st.Schedule = &ns
	saveState()
}

func markDHWOverride(now time.Time, v float64) {
	st.DHWOverride, st.DHWManual, st.SentDHW, st.SentDHWAt = now.Add(3*time.Hour), v, v, now
	log.Printf("boiler zelf op %.0f °C gezet: de regelaar volgt dat tot %s (of tot de boiler warm is)", v, st.DHWOverride.Format("15:04"))
}

// Zomerstand: warmtepomp op "alleen sanitair water" (3); bij het verlaten terug naar de vorige stand.
// Operating_Mode_State: 0 verwarmen, 3 alleen sanitair water, 4 verwarmen + sanitair water, …
func summer(now time.Time, sc Schedule, hm hmData) {
	op, ok := hm["Operating_Mode_State"]
	if !ok {
		return
	}
	if now.Sub(st.SummerAt) < 75*time.Second { // de HeishaMon toont een nieuwe stand pas na een tijdje
		return
	}
	if sc.Mode == ModeSummer && op != 3 {
		if !st.SummerOn {
			st.SummerPrev = op
		}
		logChange("summer", fmt.Sprintf("zomer: warmtepomp van stand %.0f naar alleen sanitair water", op))
		if err := sendHM("SetOperationMode", 3); err == nil {
			st.SummerOn, st.SummerAt = true, now
		}
	} else if sc.Mode != ModeSummer && !st.SummerOn && (op == 0 || op == 1 || op == 2) && !noBoiler(hm) {
		// stand zonder sanitair water (0 verwarmen, 1 koelen, 2 auto): dan negeert de warmtepomp de boiler.
		// Zet er sanitair water bij (4, 5, 6). Zet iemand hem binnen 6 uur toch weer terug, dan niet blijven vechten.
		want := op + 4
		if now.Sub(st.ModeFixAt) < 6*time.Hour {
			logChange("opmode", fmt.Sprintf("⚠ warmtepomp staat opnieuw op stand %.0f (zonder sanitair water): boiler wordt niet opgewarmd. Zet hem op de bediening op verwarmen + sanitair water.", op))
			return
		}
		logChange("opmode", fmt.Sprintf("warmtepomp stond op stand %.0f (zonder sanitair water, de boiler werd niet opgewarmd): naar stand %.0f", op, want))
		if err := sendHM("SetOperationMode", want); err == nil {
			st.ModeFixAt, st.SummerAt = now, now
		}
	} else if sc.Mode != ModeSummer && st.SummerOn {
		prev := st.SummerPrev
		if prev == 3 || prev == 0 { // niets zinnigs om naar terug te gaan: verwarmen + sanitair water
			prev = 4
		}
		if op == 3 {
			logChange("summer", fmt.Sprintf("zomer voorbij: warmtepomp terug naar stand %.0f", prev))
			if err := sendHM("SetOperationMode", prev); err != nil {
				return
			}
			st.SummerAt = now
		}
		st.SummerOn = false
	}
}

// de warmtepomp meldt uitdrukkelijk dat er geen boiler is
func noBoiler(hm hmData) bool { v, ok := hm["DHW_Installed"]; return ok && v == 0 }

// freshChange: de waarde op de warmtepomp is net veranderd, niet door de regelaar (die stuurde iets anders,
// en niet in de laatste 2 minuten): wellicht iemand aan de bediening
func freshChange(cur, seen, sent float64, sentAt, now time.Time) bool {
	return act && math.Round(cur) != math.Round(seen) && math.Round(cur) != math.Round(sent) && now.Sub(sentAt) > graceAfterSend
}

// Tot wanneer een zelf ingestelde kamertemperatuur geldt: tot de volgende verandering in het programma.
// In een periode, of tijdens het voorverwarmen ervoor: tot het einde van die periode. Daarbuiten: tot het
// programma weer iets anders wil (voorverwarmen of het begin van de volgende periode).
func nextBoundary(now time.Time, p HeatPlan) time.Time {
	sc := schedule()
	if per, ok := periodAt(sc.Heat, now); ok {
		return atMinute(now, per.To)
	}
	if !p.NextWindow.IsZero() && !now.Before(p.PreheatStart) && p.Target > sc.Eco {
		if per, ok := periodAt(sc.Heat, p.NextWindow); ok { // voorverwarmen loopt: tot het einde van die periode
			return atMinute(p.NextWindow, per.To)
		}
	}
	if !p.PreheatStart.IsZero() && p.PreheatStart.After(now) {
		return p.PreheatStart
	}
	if !p.NextWindow.IsZero() {
		return p.NextWindow
	}
	return now.Add(3 * time.Hour)
}

// ---------- bijleren ----------

// Boiler: opwarmsnelheid tijdens een beurt en afkoeling (incl. verbruik) daarbuiten, als glijdend gemiddelde.
// Verbruik per weekdag en kwartier: hoeveel zakte de boiler in een kwartier zonder opwarmen, min het
// verwachte warmteverlies. De HeishaMon geeft hele graden, dus het gemiddelde over veel weken telt (glijdend
// gemiddelde); een kwartier met 0 °C en een met 1 °C middelen samen uit. Rustige nachtkwartieren leren het
// warmteverlies zelf.
func learnDHWUse(now time.Time, temp float64, heating bool) {
	qs := now.Truncate(quarter)
	q := st.DHWQ
	if q != nil && q.Start.Equal(qs) {
		q.Heated = q.Heated || heating
		return
	}
	if q != nil && qs.Sub(q.Start) == quarter && !q.Heated && !heating {
		drop := q.Temp - temp
		l := &st.Learned
		u := l.use()
		d, i := dayIndex(q.Start), q.Start.Hour()*4+q.Start.Minute()/15
		u[d][i] = 0.75*u[d][i] + 0.25*(drop-l.lossPerQuarter(q.Temp))
		l.DHWUse = &u
		if h := q.Start.Hour(); h >= 1 && h < 5 && drop >= 0 && drop <= 1 && q.Temp > 30 {
			l.DHWLoss = clamp(0.97*l.DHWLoss+0.03*(drop*4/(q.Temp-20)), 0.002, 0.05)
		}
	}
	st.DHWQ = &dhwQuarter{Start: qs, Temp: temp, Heated: heating}
}

// opwarmblokken als tekst, bv. "03:30–05:00, 19:15–20:00"
func planBlocks(p DHWPlan) string {
	var parts []string
	for i := 0; i < len(p.Heat); {
		j := i
		for j+1 < len(p.Heat) && p.Heat[j+1].Sub(p.Heat[j]) == quarter {
			j++
		}
		parts = append(parts, p.Heat[i].Format("15:04")+"–"+p.Heat[j].Add(quarter).Format("15:04"))
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

// korte samenvatting van het plan voor het logboek: opwarmblokken en de krapste voorspelling
func planSummary(p DHWPlan) string {
	if len(p.Heat) == 0 {
		return "geen opwarming nodig; " + p.Why
	}
	return fmt.Sprintf("opwarmen %s; %s", planBlocks(p), p.Why)
}

func learnDHW(now time.Time, temp float64, heating bool) {
	m := st.DHWMark
	if m == nil || m.Heating != heating {
		st.DHWMark = &mark{now, temp, heating}
		return
	}
	h := now.Sub(m.Time).Hours()
	if heating && h >= 0.25 && temp > m.Temp {
		r := (temp - m.Temp) / h
		if r > 1 && r < 40 {
			st.Learned.DHWHeatRate = 0.8*st.Learned.DHWHeatRate + 0.2*r
		}
		st.DHWMark = &mark{now, temp, heating}
	} else if !heating && h >= 2 {
		st.DHWMark = &mark{now, temp, heating}
	}
}

// Kamer: hoe lang het duurt om van eco naar comfort te gaan, bij welke buitentemperatuur.
func learnPreheat(now time.Time, tin, tout float64, p HeatPlan, set, eco float64) {
	r := st.Preheat
	if r == nil {
		if p.Target > eco && math.Round(set) == math.Round(p.Target) && tin <= p.Target-1.5 {
			st.Preheat = &preheatRun{Start: now, Tin: tin, Target: p.Target, ToutS: tout, N: 1}
		}
		return
	}
	r.ToutS += tout
	r.N++
	switch {
	case tin >= r.Target-0.5 && now.Sub(r.Start) > 4*time.Hour:
		st.Preheat = nil // te lang: meetfout (hele graden) of de warmtepomp werkte niet mee, niet leren
	case tin >= r.Target-0.5:
		min := now.Sub(r.Start).Minutes()
		smp := PreheatSample{Time: now, Tout: r.ToutS / float64(r.N), Rise: r.Target - r.Tin, Minutes: min}
		st.Learned.Preheat = append(st.Learned.Preheat, smp)
		if n := len(st.Learned.Preheat); n > 60 {
			st.Learned.Preheat = st.Learned.Preheat[n-60:]
		}
		log.Printf("geleerd: %.1f °C bijwarmen duurde %.0f min bij %.0f °C buiten", smp.Rise, min, smp.Tout)
		st.Preheat = nil
	case math.Round(set) != math.Round(r.Target) || now.Sub(r.Start) > 8*time.Hour:
		st.Preheat = nil // onderbroken: niet meetellen
	}
}

// ---------- logboek (CSV, per maand) ----------

func writeCSV(now time.Time, hm hmData, tin, dhwWant, heatWant, price float64, roomManual, dhwManual bool, shTin, shHum float64) {
	fn := filepath.Join(dataDir, "warmte-"+now.Format("2006-01")+".csv")
	_, err := os.Stat(fn)
	f, e2 := os.OpenFile(fn, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if e2 != nil {
		return
	}
	defer f.Close()
	if os.IsNotExist(err) {
		fmt.Fprintln(f, "tijd;buiten;kamer;kamer_doel;kamer_regelaar;boiler;boiler_doel;boiler_regelaar;driewegklep;compressor_hz;warmte_w;elektrisch_w;prijs;kamer_manueel;boiler_manueel;shelly;vocht")
	}
	b01 := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	opt := func(v float64) string {
		if math.IsNaN(v) {
			return ""
		}
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	fmt.Fprintf(f, "%s;%.1f;%.1f;%.0f;%.0f;%.1f;%.0f;%.0f;%.0f;%.0f;%.0f;%.0f;%.4f;%d;%d;%s;%s\n", now.Format("2006-01-02 15:04"),
		hm["Outside_Temp"], tin, hm["Z1_Heat_Request_Temp"], heatWant, hm["DHW_Temp"], hm["DHW_Target_Temp"], dhwWant,
		hm["ThreeWay_Valve_State"], hm["Compressor_Freq"],
		hm["Heat_Power_Production"]+hm["DHW_Power_Production"], hm["Heat_Power_Consumption"]+hm["DHW_Power_Consumption"], price,
		b01(roomManual), b01(dhwManual), opt(shTin), opt(shHum))
}

// ---------- HTTP: voor evcc (boiler bij zonne-overschot) en de Stroom-app ----------

func serve() {
	h := http.NewServeMux()
	// evcc-laadpunt "Boiler": GET /boiler → true/false;  GET /boiler?on=1|0 → zonne-overschot aan/uit
	h.HandleFunc("/boiler", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if v := r.URL.Query().Get("on"); v != "" {
			on := v == "1" || v == "true"
			if on != st.PVBoost {
				log.Printf("evcc: zonne-overschot voor de boiler %v", map[bool]string{true: "aan", false: "uit"}[on])
			}
			st.PVBoost = on
			saveState()
		}
		on := st.PVBoost
		mu.Unlock()
		fmt.Fprint(w, on)
		go func() { step(time.Now()) }()
	})
	// evcc-laadpunt "Boiler": verbruik alleen tijdens een boost die evcc vroeg (anders "disabled but charging")
	h.HandleFunc("/boiler/power", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		on := st.PVBoost
		mu.Unlock()
		p := 0.0
		if hm, err := readHM(); err == nil && on {
			p = hm["DHW_Power_Consumption"]
		}
		fmt.Fprint(w, p)
	})
	// kopie van de HeishaMon-gegevens (voor evcc, de Stroom-app en de piekbewaker)
	h.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		// bij een korte wifi-onderbreking van de HeishaMon nog tot 5 min de laatste gegevens geven
		b, err := hmCached(5 * time.Minute)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	})
	// commando's doorgeven aan de HeishaMon, bv. /command?SetZ1HeatRequestTemperature=21
	h.HandleFunc("/command", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		b, err := hmGet("/command?" + r.URL.RawQuery)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		log.Printf("doorgegeven aan de HeishaMon: %s", r.URL.RawQuery)
		q := r.URL.Query()
		mu.Lock()
		now := time.Now()
		if v, err := strconv.ParseFloat(q.Get("SetDHWTemp"), 64); err == nil {
			markDHWOverride(now, v)
		}
		if v, err := strconv.ParseFloat(q.Get("SetZ1HeatRequestTemperature"), 64); err == nil {
			plan := HeatTarget(cfg, schedule(), st.Learned, HeatInput{Now: now, Tin: st.Status.Heat.Tin, Tout: st.Status.Heat.Tout, Prev: st.PrevZ1, Prices: readRates("grid")})
			st.Override, st.SentZ1, st.SentZ1At = nextBoundary(now, plan), v, now
			log.Printf("kamertemperatuur zelf op %.0f °C gezet: de regelaar wacht tot %s", v, st.Override.Format("02/01 15:04"))
		}
		saveState()
		mu.Unlock()
		w.Write(b)
		go func() { time.Sleep(hmPoll + 2*time.Second); step(time.Now()) }()
	})
	// weekschema: GET = lezen, POST = vervangen (JSON; de app stuurt text/plain, dan vraagt de browser geen toelating)
	h.HandleFunc("/schedule", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		switch r.Method {
		case http.MethodOptions:
			return
		case http.MethodPost:
			var ns Schedule
			if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&ns); err != nil {
				http.Error(w, "ongeldig schema: "+err.Error(), http.StatusBadRequest)
				return
			}
			if err := ns.Validate(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			setSchedule(ns)
			mu.Unlock()
			go func() { step(time.Now()) }()
		}
		mu.Lock()
		b, _ := json.Marshal(schedule())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	})
	// stand: /mode?set=normaal|vakantie|zomer
	h.HandleFunc("/mode", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		mu.Lock()
		ns := schedule()
		if v := r.URL.Query().Get("set"); v != "" {
			ns.Mode = v
			if err := ns.Validate(); err != nil {
				mu.Unlock()
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			setSchedule(ns)
			go func() { step(time.Now()) }()
		}
		mode := schedule().Mode
		mu.Unlock()
		fmt.Fprint(w, mode)
	})
	serveCharge(h)
	serveForecast(h)
	serveShelly(h)
	h.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		st.Status.Battery = batt
		b, _ := json.Marshal(st.Status)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write(b)
	})
	log.Fatal(http.ListenAndServe(listen, h))
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime)
	if loc, err := time.LoadLocation(env("TZ", "Europe/Brussels")); err == nil {
		time.Local = loc
	}
	// standaardschema; zodra het in de app aangepast is, geldt dat (warmteregelaar-state.json)
	sched = defaultSchedule(envFloat("COMFORT", 22), envFloat("ECO", 20), envFloat("BOILER", 40), envFloat("BOILER_AVOND", 48))
	cfg.DHWBoost = envFloat("BOILER_BOOST", cfg.DHWBoost)
	cfg.DHWBoostHot = envFloat("BOILER_BOOST_HOT", cfg.DHWBoostHot)
	if _, err := url.Parse(hmURL); err != nil {
		log.Fatal(err)
	}
	loadState()
	initLegionella(time.Now())
	log.Printf("start: HeishaMon %s, evcc %s, %s", hmURL, evccURL, map[bool]string{true: "STUURT", false: "PROEF (alleen loggen)"}[act])
	sc := schedule()
	log.Printf("stand %s · boiler gewoon %.0f °C, zonne-overschot %.0f °C (veel: %.0f °C) · kamer eco %.0f °C, vakantie %.0f °C (weekschema: /schedule)",
		sc.Mode, sc.DHWBase, cfg.DHWBoost, cfg.DHWBoostHot, sc.Eco, sc.Away)
	log.Printf("thuisbatterij: volgt het advies van de evcc-optimizer, %s", map[bool]string{true: "STUURT (BATT=1)", false: "PROEF (BATT=0, alleen loggen)"}[battAct])
	go pollHM()
	go battLoop()
	go shellyLoop()
	go serve()
	time.Sleep(3 * time.Second) // eerste HeishaMon-gegevens afwachten
	for {
		step(time.Now())
		checkChargePlan(time.Now())
		time.Sleep(tick)
	}
}
