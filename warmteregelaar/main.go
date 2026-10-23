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
	SummerOn   bool      `json:"summerOn"`
	SummerPrev float64   `json:"summerPrev"`
	SummerAt   time.Time `json:"summerAt"`
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
	Preheat *preheatRun `json:"preheat,omitempty"`
	DHWMark *mark       `json:"dhwMark,omitempty"`
	LastLog time.Time   `json:"lastLog"`
	Status  Status      `json:"-"`
}

type preheatRun struct {
	Start  time.Time `json:"start"`
	Tin    float64   `json:"tin"`
	Target float64   `json:"target"`
	ToutS  float64   `json:"toutSum"`
	N      int       `json:"n"`
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
	} `json:"dhw"`
	Heat struct {
		Tin, Tout, Set, Want float64
		Why                  string
		NextWindow           time.Time
		PreheatStart         time.Time
		LeadMinutes          float64
		Override             time.Time
	} `json:"heat"`
	Learned Learned `json:"learned"`
	Error   string  `json:"error,omitempty"`
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
	if b, err := os.ReadFile(statePath()); err == nil {
		_ = json.Unmarshal(b, &st)
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
	prices, solar := readRates("grid"), readRates("solar")
	dhw, dhwSet := hm["DHW_Temp"], hm["DHW_Target_Temp"]
	tin, tout, z1 := hm["Z1_Temp"], hm["Outside_Temp"], hm["Z1_Heat_Request_Temp"]
	dhwHeating := hm["ThreeWay_Valve_State"] == 1 && hm["Compressor_Freq"] > 0
	if tin < -50 { // geen kamersensor: dan de kamerthermostaat
		tin = hm["Room_Thermostat_Temp"]
	}

	learnDHW(now, dhw, dhwHeating)

	// boiler
	summer(now, sc, hm)

	want, why := DHWTarget(cfg, sc, st.Learned, DHWInput{Now: now, Temp: dhw, PVBoost: st.PVBoost, Prev: st.PrevDHW, Prices: prices, Solar: solar})
	st.PrevDHW = want
	// zelf aangepast op de bediening (en blijft staan): voorrang, tot de boiler warm is of hoogstens 3 uur
	if act && st.SentDHW != 0 && now.Sub(st.SentDHWAt) > graceAfterSend && math.Round(dhwSet) == math.Round(st.SeenDHW) && math.Round(dhwSet) != math.Round(st.SentDHW) &&
		math.Round(dhwSet) != math.Round(want) && !now.Before(st.DHWOverride) {
		markDHWOverride(now, dhwSet)
	}
	st.SeenDHW = dhwSet
	if now.Before(st.DHWOverride) && st.DHWManual > want && dhw >= st.DHWManual-1 {
		st.DHWOverride = time.Time{} // boiler is warm: terug naar het eigen plan
		log.Printf("boiler heeft de zelf gekozen %.0f °C bereikt: terug naar het plan", st.DHWManual)
	}
	if now.Before(st.DHWOverride) {
		want, why = st.DHWManual, "zelf ingesteld, geldt tot "+st.DHWOverride.Format("15:04")+" of tot de boiler warm is"
	}
	s.DHW.Temp, s.DHW.Target, s.DHW.Want, s.DHW.Why, s.DHW.Override = dhw, dhwSet, want, why, st.DHWOverride
	if math.Round(dhwSet) != math.Round(want) && (math.Round(st.SentDHW) != math.Round(want) || now.Sub(st.SentDHWAt) > resendAfter) {
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
	s.Learned = st.Learned

	if now.Sub(st.LastLog) >= 5*time.Minute {
		p, _ := priceAt(prices, now)
		writeCSV(now, hm, tin, want, plan.Target, p)
		st.LastLog = now
	}
	saveState()
}

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

// volgende omslag: einde/begin van een periode of voorverwarmen
func nextBoundary(now time.Time, p HeatPlan) time.Time {
	if per, ok := periodAt(schedule().Heat, now); ok {
		return atMinute(now, per.To)
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
		r := (m.Temp - temp) / h
		if r >= 0 && r < 2 { // sneller = er werd warm water getapt, dat is geen afkoeling
			st.Learned.DHWCoolRate = 0.8*st.Learned.DHWCoolRate + 0.2*r
		}
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

func writeCSV(now time.Time, hm hmData, tin, dhwWant, heatWant, price float64) {
	fn := filepath.Join(dataDir, "warmte-"+now.Format("2006-01")+".csv")
	_, err := os.Stat(fn)
	f, e2 := os.OpenFile(fn, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if e2 != nil {
		return
	}
	defer f.Close()
	if os.IsNotExist(err) {
		fmt.Fprintln(f, "tijd;buiten;kamer;kamer_doel;kamer_regelaar;boiler;boiler_doel;boiler_regelaar;driewegklep;compressor_hz;warmte_w;elektrisch_w;prijs")
	}
	fmt.Fprintf(f, "%s;%.1f;%.1f;%.0f;%.0f;%.1f;%.0f;%.0f;%.0f;%.0f;%.0f;%.0f;%.4f\n", now.Format("2006-01-02 15:04"),
		hm["Outside_Temp"], tin, hm["Z1_Heat_Request_Temp"], heatWant, hm["DHW_Temp"], hm["DHW_Target_Temp"], dhwWant,
		hm["ThreeWay_Valve_State"], hm["Compressor_Freq"],
		hm["Heat_Power_Production"]+hm["DHW_Power_Production"], hm["Heat_Power_Consumption"]+hm["DHW_Power_Consumption"], price)
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
	h.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
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
	if _, err := url.Parse(hmURL); err != nil {
		log.Fatal(err)
	}
	loadState()
	log.Printf("start: HeishaMon %s, evcc %s, %s", hmURL, evccURL, map[bool]string{true: "STUURT", false: "PROEF (alleen loggen)"}[act])
	sc := schedule()
	log.Printf("stand %s · boiler gewoon %.0f °C, boost %.0f °C · kamer eco %.0f °C, vakantie %.0f °C (weekschema: /schedule)",
		sc.Mode, sc.DHWBase, cfg.DHWBoost, sc.Eco, sc.Away)
	go pollHM()
	go serve()
	time.Sleep(3 * time.Second) // eerste HeishaMon-gegevens afwachten
	for {
		step(time.Now())
		time.Sleep(tick)
	}
}
