package main

// Sanitair warm water (SWW): de regelaar voorspelt de boilertemperatuur voor de komende 24 uur per kwartier en
// kiest zelf wanneer hij opwarmt.
//
//   temperatuur(volgend kwartier) = nu − warmteverlies − verbruik (douche, bad, afwas) + opwarmen
//
// - warmteverlies: k × (temperatuur − 20 °C), k geleerd uit rustige kwartieren ('s nachts);
// - verbruik: geleerd per weekdag en kwartier (hoeveel °C de boiler daar gemiddeld zakt);
// - opwarmen: geleerde opwarmsnelheid (°C per uur).
//
// Eisen: in een periode van het weekschema minstens de temperatuur van die periode (min 2 °C: zo werkt de
// warmtepomp ook, hij warmt pas bij te koud), daarbuiten altijd minstens de gewone temperatuur (40 °C).
// Waar de voorspelling te laag komt, voegt de regelaar het goedkoopste kwartier ervóór toe om op te warmen
// (netprijs, of bij verwachte zon de gemiste terugleververgoeding; warmer opwarmen telt duurder, want de
// warmtepomp werkt dan minder zuinig). Dat herhaalt hij tot alles klopt. Of het dus 's nachts 55 °C wordt en
// daarmee de hele dag, of 48 °C en overdag nog eens: dat volgt uit de prijzen en de zonneverwachting.

import (
	"fmt"
	"math"
	"time"
)

const (
	quarter       = 15 * time.Minute
	dhwHorizon    = 96 // kwartieren vooruit
	dhwMaxHP      = 55 // °C: hoger haalt de warmtepomp niet zonder elektrische bijverwarming
	dhwHysteresis = 2  // °C: de warmtepomp warmt pas bij zoveel onder de ingestelde temperatuur
)

// verbruiksprofiel vóór er iets geleerd is: 's ochtends wat, 's avonds douches (in het weekend meer)
func defaultDHWUse() [7][96]float64 {
	var u [7][96]float64
	for d := 0; d < 7; d++ {
		for q := 26; q < 32; q++ { // 06:30–08:00: 2 °C
			u[d][q] = 2.0 / 6
		}
		total := 5.0 // 19:00–22:00
		if d >= 5 {
			total = 8
		}
		for q := 76; q < 88; q++ {
			u[d][q] = total / 12
		}
	}
	return u
}

func (l Learned) use() [7][96]float64 {
	if l.DHWUse != nil {
		return *l.DHWUse
	}
	return defaultDHWUse()
}

// warmteverlies in °C per kwartier bij temperatuur t
func (l Learned) lossPerQuarter(t float64) float64 {
	k := l.DHWLoss
	if k <= 0 {
		k = 0.012
	}
	return clamp(k, 0.002, 0.05) * math.Max(0, t-20) / 4
}

func (l Learned) heatPerQuarter() float64 { return math.Max(2, l.DHWHeatRate) / 4 }

type DHWPlan struct {
	Target   float64     // in te stellen boilertemperatuur nu
	Planned  float64     // opwarmdoel van het plan (0 = het plan warmt nu niet op)
	Start    time.Time   // begin van het eerste kwartier van Forecast
	Why      string      // reden (voor het logboek en de app)
	Heat     []time.Time // geplande opwarmkwartieren
	MinTemp  float64     // laagste voorspelde marge t.o.v. de eis (°C)
	MinAt    time.Time
	Forecast []float64 // voorspelde temperatuur aan het einde van elk kwartier
}

// eis per kwartier: in een periode de temperatuur van de periode (min de marge), anders de gewone temperatuur
func dhwRequirement(s Schedule, t time.Time) (float64, bool) {
	if s.Mode != ModeVacation {
		if p, ok := periodAt(s.DHW, t); ok {
			return p.Temp - dhwHysteresis, true
		}
	}
	return s.DHWBase, false // buiten de periodes altijd minstens de gewone temperatuur (40 °C)
}

// kost van opwarmen in een kwartier (€/kWh, relatief): netprijs, of bij verwachte zon de terugleververgoeding
func dhwCost(in DHWInput, t time.Time, temp float64) float64 {
	p, ok := priceAt(in.Prices, t)
	if !ok {
		p = 0.30
	}
	if sun, ok := priceAt(in.Solar, t); ok && sun >= 2500 {
		if f, ok := priceAt(in.FeedIn, t); ok {
			p = f
		} else {
			p *= 0.3
		}
	}
	// warmer water maken kost meer (lagere COP): 1,5 % per °C boven 40
	return p * (1 + 0.015*math.Max(0, temp-40))
}

func simulateDHW(l Learned, use [7][96]float64, start time.Time, t0 float64, heat []bool) []float64 {
	out := make([]float64, len(heat))
	t := t0
	hq := l.heatPerQuarter()
	for i := range heat {
		qt := start.Add(time.Duration(i) * quarter)
		d, q := dayIndex(qt), qt.Hour()*4+qt.Minute()/15
		t -= l.lossPerQuarter(t) + use[d][q]
		if heat[i] {
			t = math.Min(dhwMaxHP, t+hq)
		}
		out[i] = t
	}
	return out
}

// PlanDHW: welke kwartieren opwarmen, en wat de boiler nu moet krijgen.
func PlanDHW(c Config, s Schedule, l Learned, in DHWInput) DHWPlan {
	start := in.Now.Truncate(quarter)
	use := l.use()
	if s.Mode == ModeVacation {
		use = [7][96]float64{} // niemand thuis
	}
	heat := make([]bool, dhwHorizon)
	var fc []float64
	accept := -1 // tekorten tot en met dit kwartier zijn niet op te lossen (bv. een groot bad): aanvaarden
	for iter := 0; iter < 2*dhwHorizon; iter++ {
		fc = simulateDHW(l, use, start, in.Temp, heat)
		// eerste kwartier waar de voorspelling te laag komt
		v := -1
		for i := accept + 1; i < len(fc); i++ {
			if req, _ := dhwRequirement(s, start.Add(time.Duration(i)*quarter)); fc[i] < req-1e-9 {
				v = i
				break
			}
		}
		if v < 0 {
			break
		}
		// het kwartier tot en met v met de laagste kost per °C winst op moment v (warmer opwarmen telt duurder;
		// een kwartier dat door de 55 °C-grens niets meer oplevert, telt niet); bij gelijke kost het laatste
		best, bestCost := -1, math.Inf(1)
		for i := 0; i <= v; i++ {
			if heat[i] {
				continue
			}
			before := in.Temp
			if i > 0 {
				before = fc[i-1]
			}
			heat[i] = true
			gain := simulateDHW(l, use, start, in.Temp, heat)[v] - fc[v]
			heat[i] = false
			if gain < 0.25 {
				continue
			}
			cost := dhwCost(in, start.Add(time.Duration(i)*quarter), before) / gain
			if !(i > 0 && heat[i-1]) && !(i+1 < len(heat) && heat[i+1]) {
				cost *= 1.15 // een aparte beurt (driewegklep om, compressor start): liever aansluiten
			}
			if cost <= bestCost+1e-9 {
				best, bestCost = i, cost
			}
		}
		if best < 0 {
			accept = v // niet haalbaar: zo warm als het kan, verder naar het volgende tekort
			continue
		}
		heat[best] = true
	}
	fc = simulateDHW(l, use, start, in.Temp, heat)

	plan := DHWPlan{Target: s.DHWBase, Forecast: fc, Start: start, MinTemp: math.Inf(1)}
	for i, h := range heat {
		if h {
			plan.Heat = append(plan.Heat, start.Add(time.Duration(i)*quarter))
		}
		req, _ := dhwRequirement(s, start.Add(time.Duration(i)*quarter))
		if m := fc[i] - req; m < plan.MinTemp {
			plan.MinTemp, plan.MinAt = m, start.Add(time.Duration(i+1)*quarter)
		}
	}
	tmin := fc[0]
	for _, t := range fc {
		tmin = math.Min(tmin, t)
	}
	// waar zit het krapste moment (voor de uitleg)
	need := ""
	for i := range fc {
		qt := start.Add(time.Duration(i) * quarter)
		if req, inP := dhwRequirement(s, qt); inP && fc[i]-req <= plan.MinTemp+1e-6 {
			need = fmt.Sprintf("om %s verwacht %.0f °C (nodig ≥ %.0f)", qt.Add(quarter).Format("15:04"), fc[i], req)
			break
		}
	}

	if heat[0] {
		// opwarmen tot de temperatuur aan het einde van dit aaneengesloten blok, minstens 2 °C boven nu
		end := 0
		for end+1 < len(heat) && heat[end+1] {
			end++
		}
		goal := math.Ceil(math.Max(fc[end], in.Temp+dhwHysteresis))
		plan.Target = math.Min(dhwMaxHP, math.Max(goal, s.DHWBase))
		why := fmt.Sprintf("opwarmen tot %.0f °C in %d goedkope kwartier(en)", plan.Target, end+1)
		if need != "" {
			why += ": " + need
		}
		plan.Why = why
	} else if len(plan.Heat) > 0 {
		plan.Why = fmt.Sprintf("gewoon %.0f °C; volgende opwarming om %s", s.DHWBase, plan.Heat[0].Format("15:04"))
		if need != "" {
			plan.Why += " (" + need + ")"
		}
	} else {
		plan.Why = fmt.Sprintf("gewoon %.0f °C; geen opwarming nodig (laagst verwacht %.0f °C)", s.DHWBase, tmin)
	}
	if s.Mode == ModeVacation {
		plan.Why = "vakantie: " + plan.Why
	}
	return plan
}
