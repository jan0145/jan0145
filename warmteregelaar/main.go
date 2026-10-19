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
	Learned Learned `json:"learned"`
	PVBoost bool    `json:"pvBoost"`
	// laatste doelen die de regelaar stuurde (om manuele aanpassingen te herkennen)
	SentDHW  float64   `json:"sentDHW"`
	SentZ1   float64   `json:"sentZ1"`
	PrevDHW  float64   `json:"prevDHW"`
	Override time.Time `json:"override"` // tot wanneer een manuele aanpassing van de kamertemperatuur voorgaat
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
	DHW  struct {
		Temp, Target, Want float64
		Why                string
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
	mu  sync.Mutex
	st  State
	cfg = defaultConfig()
)

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

func readHM() (hmData, error) {
	resp, err := client.Get(hmURL + "/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var d struct {
		Heatpump []struct {
			Name  string `json:"Name"`
			Value any    `json:"Value"`
		} `json:"heatpump"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
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
	resp, err := client.Get(u)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
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
	s.Time, s.Act, s.Error = now, act, ""
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
	want, why := DHWTarget(cfg, st.Learned, DHWInput{Now: now, Temp: dhw, PVBoost: st.PVBoost, Prev: st.PrevDHW, Prices: prices, Solar: solar})
	st.PrevDHW = want
	s.DHW.Temp, s.DHW.Target, s.DHW.Want, s.DHW.Why = dhw, dhwSet, want, why
	if math.Round(dhwSet) != math.Round(want) {
		logChange("dhw", fmt.Sprintf("boiler %.0f → %.0f °C: %s", dhwSet, want, why))
		if err := sendHM("SetDHWTemp", want); err == nil {
			st.SentDHW = want
		}
	}

	// verwarming (alleen als de warmtepomp met een kamertemperatuur werkt, niet met een stooklijnverschuiving)
	plan := HeatTarget(cfg, st.Learned, HeatInput{Now: now, Tin: tin, Tout: tout, Prices: prices})
	s.Heat.Tin, s.Heat.Tout, s.Heat.Set, s.Heat.Want, s.Heat.Why = tin, tout, z1, plan.Target, plan.Why
	s.Heat.NextWindow, s.Heat.PreheatStart, s.Heat.LeadMinutes = plan.NextWindow, plan.PreheatStart, plan.LeadMinutes
	if z1 >= 10 {
		// iemand heeft zelf de thermostaat aangepast: respecteren tot het volgende omslagpunt
		if act && st.SentZ1 != 0 && math.Round(z1) != math.Round(st.SentZ1) && math.Round(z1) != math.Round(plan.Target) && now.After(st.Override) {
			st.Override = nextBoundary(now, plan)
			log.Printf("kamertemperatuur zelf op %.0f °C gezet: de regelaar wacht tot %s", z1, st.Override.Format("02/01 15:04"))
		}
		if now.Before(st.Override) {
			s.Heat.Want = z1
			s.Heat.Why = "zelf ingesteld, geldt tot " + st.Override.Format("15:04")
		} else if math.Round(z1) != math.Round(plan.Target) {
			logChange("z1", fmt.Sprintf("kamer %.0f → %.0f °C: %s", z1, plan.Target, plan.Why))
			if err := sendHM("SetZ1HeatRequestTemperature", plan.Target); err == nil {
				st.SentZ1 = plan.Target
			}
		} else if act {
			st.SentZ1 = plan.Target
		}
		learnPreheat(now, tin, tout, plan, z1)
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

// volgende omslag: einde/begin van een venster of voorverwarmen
func nextBoundary(now time.Time, p HeatPlan) time.Time {
	m := now.Hour()*60 + now.Minute()
	for _, w := range cfg.windowsFor(now) {
		if m >= w.From && m < w.To {
			return atMinute(now, w.To)
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
		if r >= 0 && r < 5 {
			st.Learned.DHWCoolRate = 0.8*st.Learned.DHWCoolRate + 0.2*r
		}
		st.DHWMark = &mark{now, temp, heating}
	}
}

// Kamer: hoe lang het duurt om van eco naar comfort te gaan, bij welke buitentemperatuur.
func learnPreheat(now time.Time, tin, tout float64, p HeatPlan, set float64) {
	r := st.Preheat
	if r == nil {
		if p.Target == cfg.Comfort && math.Round(set) == math.Round(cfg.Comfort) && tin <= cfg.Comfort-1.5 {
			st.Preheat = &preheatRun{Start: now, Tin: tin, Target: cfg.Comfort, ToutS: tout, N: 1}
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
	cfg.Comfort = envFloat("COMFORT", cfg.Comfort)
	cfg.Eco = envFloat("ECO", cfg.Eco)
	cfg.DHWBase = envFloat("BOILER", cfg.DHWBase)
	cfg.DHWEvening = envFloat("BOILER_AVOND", cfg.DHWEvening)
	cfg.DHWBoost = envFloat("BOILER_BOOST", cfg.DHWBoost)
	if _, err := url.Parse(hmURL); err != nil {
		log.Fatal(err)
	}
	loadState()
	log.Printf("start: HeishaMon %s, evcc %s, %s", hmURL, evccURL, map[bool]string{true: "STUURT", false: "PROEF (alleen loggen)"}[act])
	log.Printf("boiler %.0f °C, 17–22 u ≥ %.0f °C, boost %.0f °C · kamer %.0f °C in de vensters, anders %.0f °C",
		cfg.DHWBase, cfg.DHWEvening, cfg.DHWBoost, cfg.Comfort, cfg.Eco)
	go serve()
	for {
		step(time.Now())
		time.Sleep(tick)
	}
}
