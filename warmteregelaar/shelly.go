package main

// Shelly H&T (wifi-thermometer): kamertemperatuur op 0,1 °C in plaats van de 1 °C van de warmtepomp.
// De regelaar vraagt hem elke minuut uit (SHELLY=http://192.168.0.247, leeg = niet gebruiken). Op batterij
// slaapt de Shelly en antwoordt hij niet; hij kan dan zelf zijn meting sturen met een actie in de Shelly-app:
//   http://<synology>:7080/shelly?temp=${status["temperature:0"].tC}&hum=${status["humidity:0"].rh}
// De warmtepomp regelt op zijn eigen sensor. Daarom leert de regelaar het verschil tussen beide (langzaam
// gemiddeld) en rekent hij met "Shelly + verschil": even nauwkeurig, maar op dezelfde schaal als de warmtepomp.
// Geen verse meting (30 min) → gewoon de sensor van de warmtepomp.

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var shellyURL = env("SHELLY", "http://192.168.0.247")

// Op batterij stuurt de Shelly alleen een meting als de temperatuur genoeg verandert (of om de paar uur):
// een oudere meting is dan nog altijd juist.
const shellyMaxAge = 4 * time.Hour

type shellyData struct {
	Temp float64   `json:"temp"`
	Hum  float64   `json:"hum"`
	Batt float64   `json:"batt,omitempty"` // %, 0 = onbekend
	Time time.Time `json:"time"`
}

var (
	shMu sync.Mutex
	sh   shellyData
)

func readShelly() (shellyData, error) {
	resp, err := client.Get(shellyURL + "/rpc/Shelly.GetStatus")
	if err != nil {
		return shellyData{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return shellyData{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var d struct {
		T *struct {
			C *float64 `json:"tC"`
		} `json:"temperature:0"`
		H *struct {
			RH float64 `json:"rh"`
		} `json:"humidity:0"`
		P *struct {
			Battery struct {
				Percent float64 `json:"percent"`
			} `json:"battery"`
		} `json:"devicepower:0"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return shellyData{}, err
	}
	if d.T == nil || d.T.C == nil {
		return shellyData{}, fmt.Errorf("geen temperatuur in het antwoord")
	}
	r := shellyData{Temp: *d.T.C, Time: time.Now()}
	if d.H != nil {
		r.Hum = d.H.RH
	}
	if d.P != nil {
		r.Batt = d.P.Battery.Percent
	}
	return r, nil
}

func shellyLoop() {
	if shellyURL == "" {
		return
	}
	for {
		r, err := readShelly()
		shMu.Lock()
		if err == nil {
			sh = r
		}
		msg := "Shelly bereikbaar"
		if err != nil {
			msg = "Shelly niet bereikbaar (slaapt hij? dan een actie instellen, zie README)"
			if !sh.Time.IsZero() {
				msg = "Shelly slaapt: de regelaar gebruikt de metingen die hij zelf stuurt"
			}
		}
		shMu.Unlock()
		mu.Lock()
		if lastMsg["shelly"] != msg { // alleen bij verandering
			lastMsg["shelly"] = msg
			log.Print(msg)
		}
		mu.Unlock()
		time.Sleep(time.Minute)
	}
}

// laatste verse meting
func shellyNow(now time.Time) (shellyData, bool) {
	shMu.Lock()
	defer shMu.Unlock()
	return sh, !sh.Time.IsZero() && now.Sub(sh.Time) < shellyMaxAge && sh.Temp > -40 && sh.Temp < 60
}

// roomOffset: verschil warmtepompsensor − Shelly, langzaam gemiddeld (de warmtepomp toont hele graden);
// n = aantal metingen tot nu
func roomOffset(off float64, n int, hpTemp, shTemp float64) float64 {
	d := hpTemp - shTemp
	if n == 0 {
		return clamp(d, -3, 3)
	}
	a := math.Max(0.01, 1/float64(n+1)) // eerst gewoon het gemiddelde, daarna ±100 min
	return clamp(off+a*(d-off), -3, 3)
}

// /shelly?temp=21.3&hum=55: meting die de Shelly zelf stuurt (actie in de Shelly-app). Temperatuur en
// vochtigheid mogen ook apart komen (twee acties: ?temp=${ev.tC} en ?hum=${ev.rh}).
func serveShelly(h *http.ServeMux) {
	h.HandleFunc("/shelly", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		q := r.URL.Query()
		num := func(k string) (float64, bool) {
			v, err := strconv.ParseFloat(strings.Replace(strings.TrimSpace(q.Get(k)), ",", ".", 1), 64)
			return v, err == nil
		}
		t, okT := num("temp")
		hum, okH := num("hum")
		okT = okT && t > -40 && t < 60
		okH = okH && hum >= 0 && hum <= 100
		if r.URL.RawQuery != "" {
			shMu.Lock()
			first := sh.Time.IsZero()
			if okT {
				sh.Temp, sh.Time = t, time.Now()
			}
			if okH {
				sh.Hum = hum
			}
			cur := sh
			shMu.Unlock()
			mu.Lock()
			switch {
			case !okT && !okH: // bv. ${…} niet ingevuld door de Shelly: tonen wat er binnenkwam
				if lastMsg["shellyq"] != r.URL.RawQuery {
					lastMsg["shellyq"] = r.URL.RawQuery
					log.Printf("⚠ /shelly: geen temperatuur of vochtigheid in %q (van %s): klopt de URL van de actie?", r.URL.RawQuery, r.RemoteAddr)
				}
			case okT && first:
				log.Printf("Shelly stuurt zelf zijn meting: %.1f °C", cur.Temp)
			}
			mu.Unlock()
		}
		shMu.Lock()
		b, _ := json.Marshal(sh)
		shMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	})
}
