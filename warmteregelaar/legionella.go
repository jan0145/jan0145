package main

// Legionella: het sanitair water moet minstens één keer per week ≥ 60 °C worden. Het programma van de
// warmtepomp zelf staat uit (dat koos zijn eigen moment, ook zonder zon en met de 6 kW-bijverwarming);
// de regelaar kiest nu het moment:
//   - komt de boiler bij zonne-overschot vanzelf op 60 °C, dan telt dat (geen extra beurt nodig);
//   - na 7 dagen: bij veel zon (prognose ≥ 3 kW) of overdag met een volle thuisbatterij (≥ 80 %), zodat de
//     bijverwarming (55 → 60 °C) uit zon of batterij komt en de kwartierpiek niet stijgt;
//   - na 8 dagen: in het goedkoopste kwartier van de komende 24 uur;
//   - na 9 dagen: meteen.
// Nooit beginnen als het huis al veel afneemt (kwartierpiek). Een beurt duurt tot de boiler ≥ 59 °C is,
// hoogstens 3 uur; lukt het niet, dan probeert hij het 12 uur later opnieuw.

import (
	"fmt"
	"log"
	"math"
	"time"
)

const (
	legioTemp    = 60
	legioReached = 59 // DHW_Temp heeft 1 °C resolutie
	legioEvery   = 7 * 24 * time.Hour
	legioMaxRun  = 3 * time.Hour
	legioRetry   = 12 * time.Hour
	legioSun     = 3000 // W zonneprognose
	legioBattSoc = 80
)

type legioStatus struct {
	Last time.Time `json:"last"` // laatste keer ≥ 59 °C
	Due  time.Time `json:"due"`  // vanaf wanneer een nieuwe beurt nodig is
	On   bool      `json:"on"`
	Why  string    `json:"why"`
}

// legioStart: moet de legionellabeurt nu beginnen? imp = afname uit het net (W), soc = thuisbatterij (%, -1 onbekend)
func legioStart(now, last time.Time, prices, solar []Rate, imp, soc float64) (bool, string) {
	age := now.Sub(last)
	if age < legioEvery {
		return false, ""
	}
	days := fmt.Sprintf("%.0f dagen niet ≥ 60 °C", math.Floor(age.Hours()/24))
	if imp > dhwPeakImport {
		return false, fmt.Sprintf("%s, maar het huis neemt nu %.0f W af (kwartierpiek): even wachten", days, imp)
	}
	if age >= legioEvery+2*24*time.Hour {
		return true, days + ": nu"
	}
	if sun, ok := priceAt(solar, now); ok && sun >= legioSun {
		return true, fmt.Sprintf("%s, veel zon (%.1f kW verwacht)", days, sun/1000)
	}
	if h := now.Hour(); soc >= legioBattSoc && h >= 10 && h < 17 {
		return true, fmt.Sprintf("%s, thuisbatterij %.0f %%", days, soc)
	}
	if age >= legioEvery+24*time.Hour {
		p, ok := priceAt(prices, now)
		if !ok {
			return true, days + ": geen prijzen, nu"
		}
		low := p
		for _, r := range prices {
			if r.End.After(now) && r.Start.Before(now.Add(24*time.Hour)) {
				low = math.Min(low, r.Value)
			}
		}
		if p <= low+0.0005 {
			return true, fmt.Sprintf("%s, goedkoopste kwartier (%.3f €/kWh)", days, p)
		}
		return false, days + ": wacht op het goedkoopste kwartier van de komende 24 uur"
	}
	return false, days + ": wacht op zon of een volle thuisbatterij (overdag)"
}

// legionella: houdt de beurt bij (mu moet vast zijn); geeft true als de boiler nu naar 60 °C moet
func legionella(now time.Time, dhw, dhwSet float64, prices, solar []Rate) (bool, string) {
	s := &st.Status.Legionella
	if dhw >= legioReached {
		if now.Sub(st.LegioLast) > 24*time.Hour {
			log.Printf("boiler %.0f °C: geldt als legionellabeurt (volgende ten laatste %s)", dhw, now.Add(legioEvery+2*24*time.Hour).Format("02/01"))
		}
		// warmde de warmtepomp zelf op tot 60 °C, zonder dat de regelaar het vroeg?
		if !st.LegioOn && !st.PVHotOn && math.Round(dhwSet) < legioReached-1 && lastMsg["selfhot"] == "" {
			lastMsg["selfhot"] = "gemeld"
			log.Printf("⚠ de warmtepomp warmde de boiler zelf op tot %.0f °C (ingesteld %.0f °C): staat zijn eigen legionellaprogramma nog aan?", dhw, dhwSet)
		}
		st.LegioLast = now
		if st.LegioOn {
			st.LegioOn = false
			log.Printf("legionellabeurt klaar: boiler %.0f °C na %.0f min", dhw, now.Sub(st.LegioStart).Minutes())
		}
	} else if dhw < 55 {
		lastMsg["selfhot"] = ""
	}
	switch {
	case st.LegioOn && now.Sub(st.LegioStart) > legioMaxRun:
		st.LegioOn, st.LegioFail = false, now
		log.Printf("⚠ legionellabeurt gestopt: na 3 uur pas %.0f °C (bijverwarming uit? piekbewaker?), opnieuw over 12 uur", dhw)
	case !st.LegioOn && now.Sub(st.LegioFail) >= legioRetry:
		exp, soc, _ := readExport()
		if ok, why := legioStart(now, st.LegioLast, prices, solar, -exp, soc); ok {
			st.LegioOn, st.LegioStart, st.LegioWhy = true, now, why
			log.Printf("legionellabeurt: boiler naar %d °C (%s)", legioTemp, why)
		} else {
			s.Why = why
		}
	}
	s.Last, s.Due, s.On = st.LegioLast, st.LegioLast.Add(legioEvery), st.LegioOn
	if st.LegioOn {
		s.Why = st.LegioWhy
	} else if s.Why == "" || now.Before(s.Due) {
		s.Why = "volgende beurt vanaf " + s.Due.Format("02/01")
	}
	return st.LegioOn, "legionellabeurt (wekelijks ≥ 60 °C): " + st.LegioWhy
}

// bij de eerste start: laatste keer ≥ 59 °C uit de geschiedenis, anders nu (de warmtepomp deed er net zelf een)
func initLegionella(now time.Time) {
	if !st.LegioLast.IsZero() {
		return
	}
	st.LegioLast = now
	for _, r := range readHistory(now.Add(-14*24*time.Hour), now) {
		if r.DHW >= legioReached {
			st.LegioLast = r.T
		}
	}
	log.Printf("legionella: laatste keer ≥ 60 °C op %s", st.LegioLast.Format("02/01 15:04"))
}
