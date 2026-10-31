package main

// Vaatwasser (Siemens, Home Connect-cloud). Werkwijze:
//   1. vaatwasser vullen, programma kiezen en op de vaatwasser "Fernstart" (Remote Start) aanzetten;
//   2. de regelaar kiest het moment: meteen bij zonne-overschot, anders het goedkoopste blok zodat hij klaar is
//      tegen het gekozen uur (standaard 07:00); nooit als het huis al veel afneemt (kwartierpiek);
//   3. in de Stroom-app: toestand, gepland starttijdstip, "nu starten", slim starten aan/uit, klaar tegen.
// Siemens laat een start op afstand alleen toe als Fernstart op het toestel aan staat (veiligheid).
// Instellen: HC_CLIENT_ID (app op developer.home-connect.com, Device Flow), eventueel HC_CLIENT_SECRET;
// één keer koppelen via de app (code + link). De sleutels staan in homeconnect-token.json (alleen op de Synology).
// Home Connect laat ±1000 vragen per dag toe: de regelaar vraagt elke 5–10 min, elke minuut als hij wacht om te starten.

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
)

var (
	hcClientID = env("HC_CLIENT_ID", "")
	hcSecret   = env("HC_CLIENT_SECRET", "")
	hcAPI      = strings.TrimRight(env("HC_API", "https://api.home-connect.com"), "/")
	hcHTTP     = &http.Client{Timeout: 20 * time.Second}
)

const (
	hcScope        = "IdentifyAppliance Monitor Control Settings"
	hcMediaType    = "application/vnd.bsh.sdk.v1+json"
	dishPeakImport = 3000 // W: niet starten als het huis al zoveel afneemt (verwarmen ±2 kW, grens 5 kW)
	dishSurplus    = 1500 // W teruglevering (en thuisbatterij ≥ 90 %): meteen starten
	dishDefaultMin = 180  // verwachte duur zolang hij die van het programma nog niet kent
)

// instellingen uit de app (bewaard in de toestand)
type DishSettings struct {
	Auto  bool   `json:"auto"`  // slim starten
	Ready string `json:"ready"` // "07:00": ten laatste klaar
}

func defaultDishSettings() DishSettings { return DishSettings{Auto: true, Ready: "07:00"} }

type hcToken struct {
	Access  string    `json:"access"`
	Refresh string    `json:"refresh"`
	Expires time.Time `json:"expires"`
}

// toestand voor de app
type dishStatus struct {
	Enabled   bool         `json:"enabled"` // HC_CLIENT_ID ingesteld
	Linked    bool         `json:"linked"`
	LinkCode  string       `json:"linkCode,omitempty"`
	LinkURL   string       `json:"linkUrl,omitempty"`
	Name      string       `json:"name,omitempty"`
	Connected bool         `json:"connected"`
	Op        string       `json:"op,omitempty"`      // Ready, Run, DelayedStart, Finished, Inactive, …
	Remote    bool         `json:"remote"`            // Fernstart aan
	Program   string       `json:"program,omitempty"` // bv. Dishcare.Dishwasher.Program.Eco50
	Minutes   float64      `json:"minutes,omitempty"` // verwachte duur
	Remaining float64      `json:"remaining,omitempty"`
	PlanStart time.Time    `json:"planStart,omitzero"`
	PlanEnd   time.Time    `json:"planEnd,omitzero"`
	Why       string       `json:"why,omitempty"`
	Error     string       `json:"error,omitempty"`
	Settings  DishSettings `json:"settings"`
	Calls     int          `json:"calls"` // vragen aan Home Connect vandaag
	Time      time.Time    `json:"time,omitzero"`
}

type hcOption struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
	Unit  string `json:"unit,omitempty"`
}

var (
	hcMu       sync.Mutex // alles hieronder
	hcTok      hcToken
	hcHaID     string
	dish       dishStatus
	dishSel    []hcOption // opties van het gekozen programma
	dishNow    bool       // knop "nu starten"
	hcCalls    int
	hcCallsDay int
	hcBackoff  time.Time
	dishKick   = make(chan struct{}, 1)
)

func hcTokenPath() string { return filepath.Join(dataDir, "homeconnect-token.json") }

func hcSaveToken() {
	b, _ := json.Marshal(hcTok)
	_ = os.WriteFile(hcTokenPath(), b, 0o600)
}

// ---------- aanmelden (Device Flow) en sleutels ----------

type hcError struct {
	Error struct {
		Key         string `json:"key"`
		Description string `json:"description"`
	} `json:"error"`
	OAuthError string `json:"error_description"`
}

// formulier naar /security/oauth/… (geen sleutel nodig)
func hcOAuth(path string, form url.Values) (map[string]any, error) {
	resp, err := hcHTTP.PostForm(hcAPI+path, form)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var d map[string]any
	_ = json.Unmarshal(b, &d)
	if resp.StatusCode != http.StatusOK {
		if e, ok := d["error"].(string); ok { // OAuth-fout: authorization_pending, slow_down, …
			return d, fmt.Errorf("%s", e)
		}
		return d, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return d, nil
}

func hcSetToken(d map[string]any) error {
	acc, _ := d["access_token"].(string)
	if acc == "" {
		return fmt.Errorf("geen access_token")
	}
	ref, _ := d["refresh_token"].(string)
	if ref == "" {
		ref = hcTok.Refresh // niet altijd een nieuwe
	}
	exp, _ := d["expires_in"].(float64)
	if exp <= 0 {
		exp = 3600
	}
	hcTok = hcToken{Access: acc, Refresh: ref, Expires: time.Now().Add(time.Duration(exp) * time.Second)}
	hcSaveToken()
	return nil
}

// koppelen: code en link voor de gebruiker, op de achtergrond wachten tot hij akkoord gaf
func hcLink() error {
	d, err := hcOAuth("/security/oauth/device_authorization", url.Values{"client_id": {hcClientID}, "scope": {hcScope}})
	if err != nil {
		return err
	}
	code, _ := d["device_code"].(string)
	user, _ := d["user_code"].(string)
	link, _ := d["verification_uri_complete"].(string)
	if link == "" {
		link, _ = d["verification_uri"].(string)
	}
	interval, _ := d["interval"].(float64)
	expires, _ := d["expires_in"].(float64)
	if code == "" || link == "" {
		return fmt.Errorf("onverwacht antwoord van Home Connect")
	}
	hcMu.Lock()
	dish.LinkCode, dish.LinkURL, dish.Error = user, link, ""
	hcMu.Unlock()
	log.Printf("vaatwasser: koppelen, open %s (code %s)", link, user)
	go func() {
		wait := time.Duration(math.Max(5, interval)) * time.Second
		end := time.Now().Add(time.Duration(math.Max(300, expires)) * time.Second)
		for time.Now().Before(end) {
			time.Sleep(wait)
			form := url.Values{"client_id": {hcClientID}, "grant_type": {"device_code"}, "device_code": {code}}
			if hcSecret != "" {
				form.Set("client_secret", hcSecret)
			}
			d, err := hcOAuth("/security/oauth/token", form)
			if err != nil {
				switch err.Error() {
				case "authorization_pending":
					continue
				case "slow_down":
					wait += 5 * time.Second
					continue
				}
				hcMu.Lock()
				dish.LinkCode, dish.LinkURL, dish.Error = "", "", "koppelen mislukt: "+err.Error()
				hcMu.Unlock()
				log.Printf("⚠ vaatwasser: koppelen mislukt: %v", err)
				return
			}
			hcMu.Lock()
			err = hcSetToken(d)
			dish.LinkCode, dish.LinkURL = "", ""
			if err == nil {
				dish.Linked, dish.Error = true, ""
				log.Print("vaatwasser: gekoppeld met Home Connect")
			}
			hcMu.Unlock()
			kickDish()
			return
		}
		hcMu.Lock()
		dish.LinkCode, dish.LinkURL, dish.Error = "", "", "koppelen: de code is verlopen, probeer opnieuw"
		hcMu.Unlock()
	}()
	return nil
}

// geldige sleutel (hcMu moet vast zijn)
func hcAccess() (string, error) {
	if hcTok.Refresh == "" {
		return "", fmt.Errorf("niet gekoppeld")
	}
	if hcTok.Access != "" && time.Until(hcTok.Expires) > 5*time.Minute {
		return hcTok.Access, nil
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {hcTok.Refresh}}
	if hcSecret != "" {
		form.Set("client_secret", hcSecret)
	}
	hcCalls++
	d, err := hcOAuth("/security/oauth/token", form)
	if err != nil {
		if strings.Contains(err.Error(), "invalid_grant") {
			hcTok, dish.Linked = hcToken{}, false
			hcSaveToken()
			return "", fmt.Errorf("de koppeling met Home Connect is verlopen: opnieuw koppelen in de app")
		}
		return "", fmt.Errorf("sleutel vernieuwen: %w", err)
	}
	if err := hcSetToken(d); err != nil {
		return "", err
	}
	return hcTok.Access, nil
}

// vraag aan de Home Connect-API (hcMu moet vast zijn); out mag nil zijn
func hcDo(method, path string, body any, out any) (int, error) {
	if time.Now().Before(hcBackoff) {
		return 0, fmt.Errorf("Home Connect: te veel vragen, wacht tot %s", hcBackoff.Format("15:04"))
	}
	for try := 0; try < 2; try++ {
		tok, err := hcAccess()
		if err != nil {
			return 0, err
		}
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		}
		req, _ := http.NewRequest(method, hcAPI+path, rd)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", hcMediaType)
		req.Header.Set("Accept-Language", "nl-BE")
		if body != nil {
			req.Header.Set("Content-Type", hcMediaType)
		}
		hcCalls++
		resp, err := hcHTTP.Do(req)
		if err != nil {
			return 0, err
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusUnauthorized && try == 0:
			hcTok.Access = "" // sleutel vernieuwen en opnieuw
			continue
		case resp.StatusCode == http.StatusTooManyRequests:
			s, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			hcBackoff = time.Now().Add(time.Duration(max(s, 600)) * time.Second)
			return resp.StatusCode, fmt.Errorf("Home Connect: te veel vragen vandaag, wacht tot %s", hcBackoff.Format("15:04"))
		case resp.StatusCode >= 300:
			var e hcError
			_ = json.Unmarshal(b, &e)
			msg := e.Error.Description
			if msg == "" {
				msg = strings.TrimSpace(string(b))
			}
			return resp.StatusCode, fmt.Errorf("Home Connect %d %s: %s", resp.StatusCode, e.Error.Key, msg)
		}
		if out != nil && len(b) > 0 {
			if err := json.Unmarshal(b, out); err != nil {
				return resp.StatusCode, err
			}
		}
		return resp.StatusCode, nil
	}
	return 0, fmt.Errorf("Home Connect: geen toegang")
}

// ---------- plannen ----------

// dishPlan: wanneer starten, zodat hij klaar is tegen het gekozen uur, in het goedkoopste blok (met verwachte
// zon telt de terugleverprijs, want dan gebruikt hij eigen stroom). Zonder prijzen: meteen.
func dishPlan(now time.Time, dur time.Duration, ready string, prices, feedin, solar []Rate) (time.Time, string) {
	m, err := parseHHMM(ready)
	if err != nil {
		m = 7 * 60
	}
	deadline := atMinute(now, m)
	if !deadline.After(now) {
		deadline = atMinute(now.AddDate(0, 0, 1), m)
	}
	latest := deadline.Add(-dur)
	if !latest.After(now) {
		return now, fmt.Sprintf("klaar tegen %s haalt hij niet meer: nu", deadline.Format("15:04"))
	}
	if len(prices) == 0 {
		return now, "geen prijzen: nu"
	}
	price := func(t time.Time) (float64, bool) {
		if sun, ok := priceAt(solar, t); ok && sun >= 2500 {
			if fp, ok := priceAt(feedin, t); ok {
				return fp, true
			}
		}
		return priceAt(prices, t)
	}
	cost := func(s time.Time) (float64, bool) {
		sum, n := 0.0, 0
		for t := s; t.Before(s.Add(dur)); t = t.Add(quarter) {
			p, ok := price(t)
			if !ok {
				return 0, false
			}
			sum += p
			n++
		}
		return sum / float64(n), n > 0
	}
	best, bestCost := now, math.Inf(1)
	for s := now; !s.After(latest); s = s.Truncate(quarter).Add(quarter) {
		if c, ok := cost(s); ok && c < bestCost-0.0005 {
			best, bestCost = s, c
		}
	}
	if math.IsInf(bestCost, 1) {
		return now, "geen prijzen voor die uren: nu"
	}
	sun := ""
	if s, ok := priceAt(solar, best); ok && s >= 2500 {
		sun = ", zon"
	}
	eur := strings.Replace(fmt.Sprintf("%.3f", bestCost), ".", ",", 1)
	return best, fmt.Sprintf("goedkoopste blok%s (gemiddeld %s €/kWh), klaar tegen %s", sun, eur, deadline.Format("15:04"))
}

func (st *State) dishMinutes(prog string) float64 {
	if v := st.DishDur[prog]; v > 0 {
		return v
	}
	return dishDefaultMin
}

// ---------- lus ----------

func kickDish() {
	select {
	case dishKick <- struct{}{}:
	default:
	}
}

func dishLoop() {
	hcMu.Lock()
	dish.Enabled = hcClientID != ""
	if b, err := os.ReadFile(hcTokenPath()); err == nil {
		_ = json.Unmarshal(b, &hcTok)
	}
	dish.Linked = hcTok.Refresh != ""
	hcMu.Unlock()
	if hcClientID == "" {
		return
	}
	for {
		wait := dishStep(time.Now())
		select {
		case <-time.After(wait):
		case <-dishKick:
		}
	}
}

type hcStatusResp struct {
	Data struct {
		Status []hcOption `json:"status"`
	} `json:"data"`
}

type hcProgramResp struct {
	Data struct {
		Key     string     `json:"key"`
		Options []hcOption `json:"options"`
	} `json:"data"`
}

// dishStep: één ronde; geeft terug hoe lang tot de volgende
func dishStep(now time.Time) time.Duration {
	hcMu.Lock()
	defer hcMu.Unlock()
	if d := now.YearDay(); d != hcCallsDay {
		if hcCallsDay != 0 {
			log.Printf("vaatwasser: %d vragen aan Home Connect gisteren", hcCalls)
		}
		hcCalls, hcCallsDay = 0, d
	}
	dish.Time = now
	defer func() { dish.Calls = hcCalls }()
	if !dish.Linked {
		return time.Minute
	}
	fail := func(err error) time.Duration {
		if dish.Error != err.Error() {
			log.Printf("⚠ vaatwasser: %v", err)
		}
		dish.Error = err.Error()
		return 10 * time.Minute
	}
	if hcHaID == "" {
		var d struct {
			Data struct {
				Appliances []struct {
					HaID      string `json:"haId"`
					Name      string `json:"name"`
					Type      string `json:"type"`
					Connected bool   `json:"connected"`
				} `json:"homeappliances"`
			} `json:"data"`
		}
		if _, err := hcDo(http.MethodGet, "/api/homeappliances", nil, &d); err != nil {
			return fail(err)
		}
		for _, a := range d.Data.Appliances {
			if a.Type == "Dishwasher" {
				hcHaID, dish.Name = a.HaID, a.Name
				log.Printf("vaatwasser gevonden: %s", a.Name)
			}
		}
		if hcHaID == "" {
			return fail(fmt.Errorf("geen vaatwasser in de Home Connect-account"))
		}
	}
	var sr hcStatusResp
	code, err := hcDo(http.MethodGet, "/api/homeappliances/"+hcHaID+"/status", nil, &sr)
	if err != nil {
		if code == http.StatusConflict { // toestel offline (uit of geen wifi)
			dish.Connected, dish.Op, dish.Remote, dish.Error = false, "Offline", false, ""
			dish.PlanStart, dish.PlanEnd, dish.Why = time.Time{}, time.Time{}, "niet verbonden (uit?)"
			return 10 * time.Minute
		}
		return fail(err)
	}
	dish.Connected, dish.Error = true, ""
	dish.Remote, dish.Op = false, ""
	for _, s := range sr.Data.Status {
		switch s.Key {
		case "BSH.Common.Status.OperationState":
			v, _ := s.Value.(string)
			dish.Op = v[strings.LastIndex(v, ".")+1:]
		case "BSH.Common.Status.RemoteControlStartAllowed":
			dish.Remote, _ = s.Value.(bool)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	switch dish.Op {
	case "Run", "DelayedStart", "Pause", "ActionRequired":
		dish.PlanStart, dish.PlanEnd = time.Time{}, time.Time{}
		var pr hcProgramResp
		if _, err := hcDo(http.MethodGet, "/api/homeappliances/"+hcHaID+"/programs/active", nil, &pr); err == nil {
			dish.Program = pr.Data.Key
			for _, o := range pr.Data.Options {
				if o.Key == "BSH.Common.Option.RemainingProgramTime" {
					if v, ok := o.Value.(float64); ok {
						dish.Remaining = math.Round(v / 60)
					}
				}
			}
		}
		if st.DishRunStart.IsZero() && dish.Op == "Run" {
			st.DishRunStart, st.DishRunProg = now, dish.Program
			log.Printf("vaatwasser loopt: %s", progName(dish.Program))
		}
		dish.Why = ""
		return 5 * time.Minute
	}
	// gedaan: duur van dit programma bijleren
	if !st.DishRunStart.IsZero() {
		if m := now.Sub(st.DishRunStart).Minutes(); m >= 20 && m <= 360 {
			if st.DishDur == nil {
				st.DishDur = map[string]float64{}
			}
			old := st.DishDur[st.DishRunProg]
			if old == 0 {
				old = m
			}
			st.DishDur[st.DishRunProg] = math.Round(0.6*old + 0.4*m)
			log.Printf("vaatwasser klaar: %s duurde %.0f min", progName(st.DishRunProg), m)
		}
		st.DishRunStart, dish.Remaining = time.Time{}, 0
		saveState()
	}
	if dish.Op != "Ready" || !dish.Remote {
		dish.PlanStart, dish.PlanEnd, dishSel, dishNow = time.Time{}, time.Time{}, nil, false
		dish.Why = map[bool]string{true: "klaar om te starten: zet Fernstart aan op de vaatwasser", false: ""}[dish.Op == "Ready"]
		if dish.Op == "Ready" {
			return 5 * time.Minute
		}
		return 10 * time.Minute
	}
	// Fernstart staat aan: het gekozen programma ophalen (één keer per beurt)
	if dishSel == nil {
		var pr hcProgramResp
		if _, err := hcDo(http.MethodGet, "/api/homeappliances/"+hcHaID+"/programs/selected", nil, &pr); err != nil {
			return fail(err)
		}
		dish.Program, dishSel = pr.Data.Key, []hcOption{}
		for _, o := range pr.Data.Options {
			if dishOptionSettable(o.Key) {
				dishSel = append(dishSel, o)
			}
		}
		log.Printf("vaatwasser: Fernstart aan, programma %s", progName(dish.Program))
	}
	dish.Minutes = st.dishMinutes(dish.Program)
	dur := time.Duration(dish.Minutes) * time.Minute
	set := st.Dish
	exp, soc, okExp := readExport()
	start, why := dishPlan(now, dur, set.Ready, readRates("grid"), readRates("feedin"), readRates("solar"))
	dish.PlanStart, dish.PlanEnd, dish.Why = start, start.Add(dur), why
	doStart := false
	switch {
	case dishNow:
		doStart, why = true, "zelf gestart in de app"
	case !set.Auto:
		dish.PlanStart, dish.PlanEnd, dish.Why = time.Time{}, time.Time{}, "slim starten staat uit: start hem in de app of op de vaatwasser"
		return 5 * time.Minute
	case okExp && exp >= dishSurplus && (soc < 0 || soc >= 90):
		doStart, why = true, fmt.Sprintf("zonne-overschot (%.1f kW naar het net)", exp/1000)
	case !start.After(now.Add(time.Minute)):
		doStart = true
	}
	if doStart && !dishNow && okExp && -exp > dishPeakImport && now.Before(start.Add(30*time.Minute)) {
		dish.Why = fmt.Sprintf("wacht even: het huis neemt nu %.0f W af (kwartierpiek)", -exp)
		return time.Minute
	}
	if !doStart {
		return time.Minute
	}
	body := map[string]any{"data": map[string]any{"key": dish.Program, "options": dishSel}}
	if _, err := hcDo(http.MethodPut, "/api/homeappliances/"+hcHaID+"/programs/active", body, nil); err != nil {
		dishNow = false
		dish.Why = "starten mislukt"
		return fail(err)
	}
	log.Printf("vaatwasser gestart: %s, %s (verwacht klaar ±%s)", progName(dish.Program), why, now.Add(dur).Format("15:04"))
	dish.Op, dish.Why, dishNow, dishSel = "Run", "", false, nil
	dish.PlanStart, dish.PlanEnd = time.Time{}, time.Time{}
	st.DishRunStart, st.DishRunProg = now, dish.Program
	saveState()
	return time.Minute
}

// opties die we mee terugsturen bij het starten (wat de gebruiker koos), niet de tellers en schattingen
func dishOptionSettable(key string) bool {
	for _, x := range []string{"Time", "ProgramProgress", "Forecast", "EnergyForecast", "WaterForecast"} {
		if strings.Contains(key, x) {
			return false
		}
	}
	return true
}

// "Dishcare.Dishwasher.Program.Eco50" → "Eco50"
func progName(key string) string {
	if key == "" {
		return "?"
	}
	return key[strings.LastIndex(key, ".")+1:]
}

func serveDish(h *http.ServeMux) {
	// GET: toestand; POST (JSON als text/plain): {"auto":true,"ready":"07:00"} en/of {"action":"start"|"link"}
	h.HandleFunc("/vaatwasser", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodPost {
			var in struct {
				Action string        `json:"action"`
				Set    *DishSettings `json:"settings"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
				http.Error(w, "ongeldig: "+err.Error(), http.StatusBadRequest)
				return
			}
			if in.Set != nil {
				if _, err := parseHHMM(in.Set.Ready); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				mu.Lock()
				st.Dish = *in.Set
				saveState()
				mu.Unlock()
				log.Printf("vaatwasser: slim starten %s, klaar tegen %s", map[bool]string{true: "aan", false: "uit"}[in.Set.Auto], in.Set.Ready)
			}
			switch in.Action {
			case "link":
				if hcClientID == "" {
					http.Error(w, "HC_CLIENT_ID is niet ingesteld in de taak van de warmteregelaar", http.StatusBadRequest)
					return
				}
				if err := hcLink(); err != nil {
					http.Error(w, "koppelen: "+err.Error(), http.StatusBadGateway)
					return
				}
			case "start":
				hcMu.Lock()
				dishNow = true
				hcMu.Unlock()
			}
			kickDish()
		}
		hcMu.Lock()
		d := dish
		hcMu.Unlock()
		mu.Lock()
		d.Settings = st.Dish
		mu.Unlock()
		b, _ := json.Marshal(d)
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	})
}
