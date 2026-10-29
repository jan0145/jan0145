package main

// Boiler forceren: de Panasonic reageert niet op een hogere gevraagde boilertemperatuur. Hij begint pas als de
// boiler zelf genoeg afkoelt (soms uren later). Vraagt de regelaar opwarmen en doet de warmtepomp na 10 minuten
// nog niets, dan zet hij "sanitair water forceren" aan (HeishaMon SetForceDHW=1), en weer uit zodra de boiler
// warm is, de regelaar niet meer opwarmen vraagt, of na 2 uur.
// Proef: alles wat de warmtepomp dan doet, staat in het logboek ("forceren: …"). Gaat de elektrische
// bijverwarming aan terwijl de warmtepomp het zelf kan (≤ 55 °C), dan stopt hij en forceert hij niet meer
// (FORCE_DHW=0 zet het helemaal uit; na een nakijken weer aan met FORCE_DHW=1 en de toestand wissen).

import (
	"fmt"
	"log"
	"math"
	"time"
)

var forceDHWOn = env("FORCE_DHW", "1") == "1"

const (
	forceAfter  = 10 * time.Minute // zo lang wachten of de warmtepomp vanzelf begint
	forceMaxRun = 2 * time.Hour
)

// forceDHW (mu moet vast zijn); geeft een korte toelichting voor de status ("" = niet geforceerd)
func forceDHW(now time.Time, hm hmData, dhw, dhwSet, want float64) string {
	valve := hm["ThreeWay_Valve_State"] == 1
	hz := hm["Compressor_Freq"]
	heater := hm["Internal_Heater_State"] == 1 || hm["External_Heater_State"] == 1
	state := fmt.Sprintf("klep %s, compressor %.0f Hz, bijverwarming %s, boiler %.0f/%.0f °C, Force_DHW_State %v",
		map[bool]string{true: "boiler", false: "verwarming"}[valve], hz, map[bool]string{true: "AAN", false: "uit"}[heater], dhw, dhwSet, hm["Force_DHW_State"])

	if st.ForceOn {
		if lastMsg["force"] != state { // proef: elke verandering loggen
			lastMsg["force"] = state
			log.Printf("forceren: %s", state)
		}
		stop := ""
		switch {
		case heater && dhwSet <= dhwMaxHP:
			stop = "⚠ de elektrische bijverwarming ging aan, terwijl de warmtepomp dit zelf kan: niet meer forceren (FORCE_DHW)"
			st.ForceBad = true
		case dhw >= math.Min(dhwSet, want)-1:
			stop = "boiler warm"
		case math.Round(want) < math.Round(dhwSet):
			stop = "de regelaar vraagt geen opwarmen meer"
		case now.Sub(st.ForceAt) > forceMaxRun:
			stop = "na 2 uur nog niet warm"
		}
		if stop == "" {
			return fmt.Sprintf("geforceerd sinds %s", st.ForceAt.Format("15:04"))
		}
		if err := sendHM("SetForceDHW", 0); err == nil || err == errProef {
			log.Printf("forceren uit (%s) na %.0f min: %s", stop, now.Sub(st.ForceAt).Minutes(), state)
			st.ForceOn, lastMsg["force"] = false, ""
		}
		return ""
	}
	if !forceDHWOn || st.ForceBad || !act || hm["Heatpump_State"] != 1 || noBoiler(hm) {
		return ""
	}
	// de warmtepomp heeft ons doel, maar warmt de boiler al 10 min niet op
	if st.DHWAskSince.IsZero() || now.Sub(st.DHWAskSince) < forceAfter || math.Round(dhwSet) != math.Round(want) {
		return ""
	}
	if exp, _, ok := readExport(); ok && -exp > dhwPeakImport {
		logChange("forcewait", fmt.Sprintf("forceren uitgesteld: het huis neemt nu %.0f W af (kwartierpiek)", -exp))
		return ""
	}
	if err := sendHM("SetForceDHW", 1); err != nil {
		return ""
	}
	st.ForceOn, st.ForceAt, lastMsg["force"] = true, now, state
	log.Printf("forceren aan: de warmtepomp warmt de boiler al %.0f min niet op (%s)", now.Sub(st.DHWAskSince).Minutes(), state)
	return "geforceerd"
}
