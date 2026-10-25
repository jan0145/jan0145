package main

// Laadplan voor de auto: evcc kent zonder voertuig alleen een eenmalig plan ("x kWh tegen hh:mm").
// De regelaar zet dat plan elke keer opnieuw als de auto ingeplugd is, zodat het elke nacht geldt.
// Daarnaast geeft hij opdrachten van de Stroom-app door aan evcc die een browser niet rechtstreeks
// mag sturen (DELETE, bv. een plan of prijsgrens wissen).

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type ChargePlan struct {
	On   bool    `json:"on"`
	KWh  float64 `json:"kwh"`
	Time string  `json:"time"` // "07:00"
}

func defaultChargePlan() ChargePlan { return ChargePlan{KWh: 16, Time: "07:00"} }

func (p *ChargePlan) Validate() error {
	if p.KWh < 1 || p.KWh > 100 {
		return fmt.Errorf("laaddoel 1–100 kWh")
	}
	m, err := parseHHMM(p.Time)
	if err != nil || m >= 1440 {
		return fmt.Errorf("ongeldig uur %q", p.Time)
	}
	return nil
}

// nextPlanTime: het eerstvolgende tijdstip "hh:mm" na now (vandaag of morgen).
func nextPlanTime(now time.Time, hhmmStr string) time.Time {
	m, _ := parseHHMM(hhmmStr)
	t := atMinute(now, m)
	if !t.After(now.Add(30 * time.Minute)) { // te kort dag: dan morgen
		t = atMinute(now.AddDate(0, 0, 1), m)
	}
	return t
}

type evccLoadpoint struct {
	Title                 string    `json:"title"`
	Connected             bool      `json:"connected"`
	Enabled               bool      `json:"enabled"`  // evcc laat laden
	Charging              bool      `json:"charging"` // de auto neemt stroom
	ChargePower           float64   `json:"chargePower"`
	ChargedEnergy         float64   `json:"chargedEnergy"` // Wh in deze laadbeurt
	ChargerFeatureHeating bool      `json:"chargerFeatureHeating"`
	PlanEnergy            float64   `json:"planEnergy"`
	PlanTime              time.Time `json:"planTime"`
}

// de laadpaal (geen boiler) en zijn nummer in de evcc-API (1, 2, …)
func readCarLoadpoint() (int, evccLoadpoint, error) {
	resp, err := client.Get(evccURL + "/api/state")
	if err != nil {
		return 0, evccLoadpoint{}, err
	}
	defer resp.Body.Close()
	var d struct {
		Loadpoints []evccLoadpoint `json:"loadpoints"`
		Result     struct {
			Loadpoints []evccLoadpoint `json:"loadpoints"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return 0, evccLoadpoint{}, err
	}
	lps := d.Loadpoints
	if len(lps) == 0 {
		lps = d.Result.Loadpoints
	}
	for i, lp := range lps {
		if !lp.ChargerFeatureHeating {
			return i + 1, lp, nil
		}
	}
	return 0, evccLoadpoint{}, fmt.Errorf("evcc kent geen laadpaal")
}

func evccDo(method, path string) ([]byte, int, error) {
	req, err := http.NewRequest(method, evccURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return b, resp.StatusCode, nil
}

// checkChargePlan: auto ingeplugd en nog geen plan → plan zetten, één keer per vertrektijd
// (wist iemand het plan zelf, dan zet de regelaar het voor die nacht niet opnieuw).
func checkChargePlan(now time.Time) {
	if _, lp, err := readCarLoadpoint(); err == nil {
		mu.Lock()
		idle, full := carFull(st.CarIdleSince, st.CarFull, now, lp)
		if full && !st.CarFull {
			st.CarFullAt = now
			log.Printf("auto vol: hij neemt al 10 min geen stroom meer terwijl evcc laadt (%.1f kWh geladen)", lp.ChargedEnergy/1000)
		}
		st.CarIdleSince, st.CarFull = idle, full
		mu.Unlock()
	}
	setChargePlan(now, false)
}

// carFull: is de auto zeker vol? evcc laat laden, er werd in deze laadbeurt al geladen (≥ 0,5 kWh), en de auto
// neemt al 10 minuten geen stroom meer. Een pauze voor de kwartierpiek (evcc laat dan niet laden) of een
// laadpaal die niet start (niets geladen) telt dus niet. "Vol" blijft tot de auto weer laadt of uitgeplugd wordt.
func carFull(idleSince time.Time, wasFull bool, now time.Time, lp evccLoadpoint) (time.Time, bool) {
	switch {
	case !lp.Connected, lp.ChargePower > 300:
		return time.Time{}, false
	case lp.Enabled && lp.ChargedEnergy >= 500 && lp.ChargePower < 100:
		if idleSince.IsZero() {
			idleSince = now
		}
		return idleSince, wasFull || now.Sub(idleSince) >= 10*time.Minute
	}
	return time.Time{}, wasFull
}

// setChargePlan: force = ook een bestaand plan vervangen (het nachtdoel werd net aangepast)
func setChargePlan(now time.Time, force bool) {
	mu.Lock()
	p := st.ChargePlan
	mu.Unlock()
	if !p.On {
		return
	}
	id, lp, err := readCarLoadpoint()
	if err != nil || !lp.Connected || (lp.PlanEnergy > 0 && !force) {
		return
	}
	at := nextPlanTime(now, p.Time)
	mu.Lock()
	done := st.ChargePlanFor.Equal(at)
	mu.Unlock()
	if done && !force {
		return
	}
	path := fmt.Sprintf("/api/loadpoints/%d/plan/energy/%g/%s", id, p.KWh, at.UTC().Format(time.RFC3339))
	b, code, err := evccDo(http.MethodPost, path)
	if err != nil || code != http.StatusOK {
		log.Printf("⚠ laadplan niet gezet (%d %v): %s", code, err, strings.TrimSpace(string(b)))
		return
	}
	log.Printf("laadplan gezet: %g kWh tegen %s", p.KWh, at.Format("02/01 15:04"))
	mu.Lock()
	st.ChargePlanFor = at
	saveState()
	mu.Unlock()
}

// alleen deze evcc-opdrachten mag de app via de regelaar sturen
func evccProxyAllowed(method, path string) bool {
	if method != http.MethodPost && method != http.MethodDelete {
		return false
	}
	for _, pre := range []string{"/api/loadpoints/", "/api/batterygridchargelimit", "/api/smartcostlimit"} {
		if strings.HasPrefix(path, pre) && !strings.Contains(path, "..") {
			return true
		}
	}
	return false
}

func serveCharge(h *http.ServeMux) {
	// laadplan: GET = lezen, POST = vervangen (JSON als text/plain)
	h.HandleFunc("/laadplan", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodPost {
			var p ChargePlan
			if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&p); err != nil {
				http.Error(w, "ongeldig laadplan: "+err.Error(), http.StatusBadRequest)
				return
			}
			if err := p.Validate(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			st.ChargePlan, st.ChargePlanFor = p, time.Time{}
			saveState()
			mu.Unlock()
			log.Printf("laadplan: %s", map[bool]string{true: fmt.Sprintf("elke nacht %g kWh tegen %s", p.KWh, p.Time), false: "uit"}[p.On])
			go setChargePlan(time.Now(), p.On)
		}
		mu.Lock()
		b, _ := json.Marshal(struct {
			ChargePlan
			Full      bool      `json:"full"` // auto zeker vol
			FullSince time.Time `json:"fullSince,omitzero"`
		}{st.ChargePlan, st.CarFull, st.CarFullAt})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	})
	// /evcc?m=DELETE&p=/api/loadpoints/1/plan/energy
	h.HandleFunc("/evcc", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		m, p := strings.ToUpper(r.URL.Query().Get("m")), r.URL.Query().Get("p")
		if !evccProxyAllowed(m, p) {
			http.Error(w, "niet toegelaten", http.StatusForbidden)
			return
		}
		b, code, err := evccDo(m, p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		log.Printf("doorgegeven aan evcc: %s %s (%d)", m, p, code)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		w.Write(b)
	})
}
