package main

// Thuisbatterij: de optimizer van evcc rekent uit wanneer de batterij best uit het net laadt, de lading
// vasthoudt of gewoon werkt (prijzen, zonneprognose, verwacht verbruik, kwartierpiek), maar voert dat in
// evcc 0.316 nog niet zelf uit ("alleen advies"). De regelaar volgt dat advies:
//   BATT=0 (standaard): proef, alleen loggen wat hij zou doen;  BATT=1: de batterijmodus van evcc zetten.
// evcc zet een extern gezette modus na 1 minuut zelf terug naar normaal; de regelaar herhaalt hem dus elke
// 30 s. Valt de regelaar weg, dan werkt de batterij binnen een minuut weer gewoon.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"
)

var battAct = env("BATT", "0") == "1"

// bij zoveel afname nooit extra uit het net laden (kwartierpiek-grens 5 kW, met marge)
const battMaxImport = 4500

// laadvermogen van de batterij uit het net (maxchargepower in evcc.yaml)
var battChargeW = envFloat("BATT_CHARGE", 3500)

// toestand in huis voor de beslissing (W)
type battHouse struct {
	Grid float64 // afname (+) / injectie (−)
	Batt float64 // batterij: ontladen (+) / laden (−)
	Car  float64 // laadvermogen van de auto
}

// verbruik zonder de batterij: wat het net zou leveren als de batterij niets doet
func (h battHouse) base() float64 { return h.Grid + h.Batt }

type battState struct {
	Advice  string    `json:"advice"`  // advies van de optimizer: normal, charge, hold, holdcharge, discharge ("" = geen)
	Applied string    `json:"applied"` // wat de regelaar nu in evcc zet ("" = niets, evcc beslist zelf)
	Act     bool      `json:"act"`
	Why     string    `json:"why"`
	Time    time.Time `json:"time"`
}

var batt battState

type evccBatt struct {
	Devs []struct {
		Name         string `json:"name"`
		Controllable bool   `json:"controllable"`
		Suggestion   *struct {
			Action string `json:"action"`
		} `json:"suggestion"`
	} `json:"devs"`
	Grid  *float64 `json:"grid"`
	GridP *float64 `json:"gridPower"`
	Batt  *float64 `json:"batt"`
	BattP *float64 `json:"battPower"`
	Car   *float64 `json:"car"`
}

func readBattAdvice() (string, battHouse, error) {
	var h battHouse
	q := "{devs: [.battery.devices[]? | {name, controllable, suggestion}], grid: .grid.power, gridPower: .gridPower, batt: .battery.power, battPower: .batteryPower, " +
		"car: ([.loadpoints[]? | select(.chargerFeatureHeating | not) | .chargePower // 0] | add)}"
	resp, err := client.Get(evccURL + "/api/state?jq=" + url.QueryEscape(q))
	if err != nil {
		return "", h, err
	}
	defer resp.Body.Close()
	var d struct {
		evccBatt
		Result evccBatt `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return "", h, err
	}
	b := d.evccBatt
	if len(b.Devs) == 0 && b.Grid == nil && b.GridP == nil {
		b = d.Result
	}
	first := func(vs ...*float64) float64 {
		for _, v := range vs {
			if v != nil {
				return *v
			}
		}
		return 0
	}
	h = battHouse{Grid: first(b.Grid, b.GridP), Batt: first(b.Batt, b.BattP), Car: first(b.Car)}
	for _, dv := range b.Devs {
		if dv.Controllable && dv.Suggestion != nil {
			return dv.Suggestion.Action, h, nil
		}
	}
	return "", h, nil
}

// battDecide: welke modus de regelaar zet ("" = niets, evcc werkt gewoon). Gerekend met het verbruik zonder de
// batterij (h.base), zodat de beslissing niet heen en weer springt door wat de batterij zelf doet:
//   - laden uit het net alleen als huis + auto + laden onder 4,5 kW blijft (kwartierpiek);
//   - zit het huis zelf al boven 4,5 kW, dan gewone werking: de batterij mag de piek afvlakken;
//   - laadt de auto, dan de batterij niet naar de auto laten ontladen (tenzij voor de piek).
func battDecide(advice string, h battHouse) (string, string) {
	base := h.base()
	peak := base > battMaxImport
	var mode, why string
	switch advice {
	case "charge":
		switch {
		case peak:
			mode, why = "", fmt.Sprintf("optimizer: laden uit het net, maar het huis neemt al %.0f W (kwartierpiek): gewone werking", base)
		case base+battChargeW > battMaxImport:
			mode, why = "hold", fmt.Sprintf("optimizer: laden uit het net, maar huis + laden zou %.0f W zijn (kwartierpiek): vasthouden", base+battChargeW)
		default:
			mode, why = "charge", "optimizer: laden uit het net (goedkoop nu, later duurder of geen zon)"
		}
	case "hold":
		if peak {
			mode, why = "", fmt.Sprintf("optimizer: niet ontladen, maar het huis neemt %.0f W (kwartierpiek): gewone werking", base)
		} else {
			mode, why = "hold", "optimizer: niet ontladen (de lading is later meer waard)"
		}
	case "holdcharge":
		mode, why = "holdcharge", "optimizer: niet laden (zonnestroom nu beter teruggeleverd of later gebruikt)"
	case "discharge":
		why = "optimizer: ontladen naar het net (niet toegelaten): gewone werking"
	case "normal":
		why = "optimizer: gewone werking"
	default:
		why = "geen advies van de optimizer (staat hij aan in evcc?)"
	}
	if mode == "" && h.Car > 1000 && !peak {
		mode, why = "hold", fmt.Sprintf("de auto laadt (%.1f kW): niet uit de thuisbatterij", h.Car/1000)
	}
	return mode, why
}

// geen heen-en-weer rond een grens: pas weer laden uit het net als de vorige modus al 5 min geldt, en vasthouden
// niet meteen loslaten. Stoppen met laden of ingrijpen voor de kwartierpiek gebeurt wel meteen.
const battMinHold = 5 * time.Minute

func battSmooth(prev string, prevAt time.Time, mode string, now time.Time, peak bool) string {
	if mode == prev || peak || now.Sub(prevAt) >= battMinHold {
		return mode
	}
	if mode == "charge" || (prev == "hold" && mode == "") {
		return prev
	}
	return mode
}

var (
	battPrev   string
	battPrevAt time.Time
)

func battLoop() {
	for {
		advice, house, err := readBattAdvice()
		now := time.Now()
		var mode, why string
		if err != nil {
			mode, why = "", "evcc niet bereikbaar: "+err.Error()
		} else {
			mode, why = battDecide(advice, house)
			if m := battSmooth(battPrev, battPrevAt, mode, now, house.base() > battMaxImport); m != mode {
				mode, why = m, why+fmt.Sprintf(" (nog %s: niet heen en weer)", map[bool]string{true: "modus " + m, false: "gewone werking"}[m != ""])
			}
		}
		if mode != battPrev {
			battPrev, battPrevAt = mode, now
		}
		prev := batt.Applied
		if battAct {
			switch {
			case mode != "":
				if _, code, err := evccDo(http.MethodPost, "/api/batterymode/"+mode); err != nil || code != http.StatusOK {
					why += fmt.Sprintf(" (⚠ niet gelukt: %d %v)", code, err)
					mode = ""
				}
			case prev != "":
				evccDo(http.MethodDelete, "/api/batterymode") // terug naar evcc zelf
			}
		}
		mu.Lock()
		batt = battState{Advice: advice, Applied: map[bool]string{true: mode, false: ""}[battAct], Act: battAct, Why: why, Time: now}
		var msg string
		if battAct {
			msg = fmt.Sprintf("batterij: %s → %s", why, map[bool]string{true: "modus " + mode, false: "evcc beslist zelf"}[mode != ""])
		} else {
			msg = fmt.Sprintf("batterij (proef, niets verstuurd): %s%s", why, map[bool]string{true: " → zou modus " + mode + " zetten", false: ""}[mode != ""])
		}
		if lastMsg["batt"] != msg { // alleen loggen als het verandert
			lastMsg["batt"] = msg
			log.Print(msg)
		}
		mu.Unlock()
		time.Sleep(30 * time.Second)
	}
}
