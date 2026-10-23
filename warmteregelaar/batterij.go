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
}

func readBattAdvice() (string, float64, error) {
	q := "{devs: [.battery.devices[]? | {name, controllable, suggestion}], grid: .grid.power, gridPower: .gridPower}"
	resp, err := client.Get(evccURL + "/api/state?jq=" + url.QueryEscape(q))
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	var d struct {
		evccBatt
		Result evccBatt `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return "", 0, err
	}
	b := d.evccBatt
	if len(b.Devs) == 0 {
		b = d.Result
	}
	grid := 0.0
	if b.Grid != nil {
		grid = *b.Grid
	} else if b.GridP != nil {
		grid = *b.GridP
	}
	for _, dv := range b.Devs {
		if dv.Controllable && dv.Suggestion != nil {
			return dv.Suggestion.Action, grid, nil
		}
	}
	return "", grid, nil
}

// battDecide: welke modus de regelaar zet ("" = niets, evcc werkt gewoon)
func battDecide(advice string, grid float64) (string, string) {
	switch advice {
	case "charge":
		if grid > battMaxImport {
			return "hold", fmt.Sprintf("optimizer: laden uit het net, maar de afname is al %.0f W (kwartierpiek): even vasthouden", grid)
		}
		return "charge", "optimizer: laden uit het net (goedkoop nu, later duurder of geen zon)"
	case "hold":
		return "hold", "optimizer: niet ontladen (de lading is later meer waard)"
	case "holdcharge":
		return "holdcharge", "optimizer: niet laden (zonnestroom nu beter teruggeleverd of later gebruikt)"
	case "discharge":
		return "", "optimizer: ontladen naar het net (niet toegelaten): gewone werking"
	case "normal":
		return "", "optimizer: gewone werking"
	}
	return "", "geen advies van de optimizer (staat hij aan in evcc?)"
}

func battLoop() {
	for {
		advice, grid, err := readBattAdvice()
		now := time.Now()
		var mode, why string
		if err != nil {
			mode, why = "", "evcc niet bereikbaar: "+err.Error()
		} else {
			mode, why = battDecide(advice, grid)
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
