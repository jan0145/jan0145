package main

// Beslissingen van de warmteregelaar: welke boiler- en kamertemperatuur moet de warmtepomp nu krijgen.
// Alles hier is zuiver rekenwerk (geen netwerk), zodat het met tests na te rekenen is.

import (
	"fmt"
	"math"
	"time"
)

type Rate struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Value float64   `json:"value"`
}

type Config struct {
	// sanitair warm water (de temperaturen per periode staan in het weekschema; het plan in sww.go)
	DHWBoost    float64 // bij zonne-overschot
	DHWBoostHot float64 // bij veel overschot of een negatieve terugleverprijs (met de elektrische bijverwarming)
	// verwarming
	MaxEarlyMinutes     int     // hoeveel vroeger dan nodig mag voorverwarmen starten als het goedkoper is
	EarlyPenaltyPerHour float64 // vroeger starten kost meer energie: zoveel % duurder per uur
}

func defaultConfig() Config {
	return Config{DHWBoost: 55, DHWBoostHot: 60, MaxEarlyMinutes: 60, EarlyPenaltyPerHour: 0.15}
}

// Wat de regelaar geleerd heeft over de boiler en het huis.
type Learned struct {
	DHWHeatRate float64         `json:"dhwHeatRate"`      // °C per uur tijdens een boilerbeurt
	DHWCoolRate float64         `json:"dhwCoolRate"`      // (oud, niet meer gebruikt)
	DHWLoss     float64         `json:"dhwLoss"`          // warmteverlies: °C per uur per °C boven 20 °C
	DHWUse      *[7][96]float64 `json:"dhwUse,omitempty"` // verbruik: °C per kwartier, per weekdag (0 = maandag)
	RoomLoss    float64         `json:"roomLoss"`         // afkoeling huis: °C per uur per °C verschil met buiten
	Preheat     []PreheatSample `json:"preheat"`          // hoe lang opwarmen duurde, bij welke buitentemperatuur
}

type PreheatSample struct {
	Time    time.Time `json:"time"`
	Tout    float64   `json:"tout"`
	Rise    float64   `json:"rise"` // °C die moest worden bijgewarmd
	Minutes float64   `json:"minutes"`
}

func defaultLearned() Learned { return Learned{DHWHeatRate: 8, DHWLoss: 0.012} }

// LeadMinutes: hoeveel minuten voor het venster de verwarming hoger moet om op tijd warm te zijn.
// Zonder metingen een voorzichtige schatting; met ≥ 4 metingen een rechte door de metingen
// (minuten per °C bijwarmen, afhankelijk van de buitentemperatuur).
func (l Learned) LeadMinutes(tout, rise float64) float64 {
	if rise <= 0 {
		return 0
	}
	cold := math.Max(0, 15-tout)
	// metingen van meer dan 4 uur zijn meetfouten (de kamer wordt in hele graden gemeten): niet meetellen
	var ok []PreheatSample
	for _, s := range l.Preheat {
		if s.Rise > 0 && s.Minutes <= 240 {
			ok = append(ok, s)
		}
	}
	if len(ok) < 4 {
		return clamp((30+4*cold)*rise, 45, 240)
	}
	// minuten per °C = a + b·cold, kleinste kwadraten
	var n, sx, sy, sxx, sxy float64
	for _, s := range ok {
		x, y := math.Max(0, 15-s.Tout), s.Minutes/s.Rise
		n++
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	a, b := sy/n, 0.0
	if d := n*sxx - sx*sx; d > 1e-9 {
		b = math.Max(0, (n*sxy-sx*sy)/d)
		a = (sy - b*sx) / n
	}
	return clamp((a+b*cold)*rise*1.1, 20, 360) // 10 % marge
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func atMinute(day time.Time, m int) time.Time {
	y, mo, d := day.Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, day.Location()).Add(time.Duration(m) * time.Minute)
}

// priceAt: prijs van het kwartier waarin t valt (ok=false als onbekend).
func priceAt(rates []Rate, t time.Time) (float64, bool) {
	for _, r := range rates {
		if !t.Before(r.Start) && t.Before(r.End) {
			return r.Value, true
		}
	}
	return 0, false
}

func avgPrice(rates []Rate, from, to time.Time) (float64, bool) {
	var sum float64
	var n int
	for t := from; t.Before(to); t = t.Add(15 * time.Minute) {
		if p, ok := priceAt(rates, t); ok {
			sum += p
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

type DHWInput struct {
	Now     time.Time
	Temp    float64
	PVBoost bool    // evcc: zonne-overschot voor de boiler
	PVHot   bool    // veel overschot of negatieve terugleverprijs: tot DHWBoostHot
	Prev    float64 // opwarmdoel van het plan in de vorige ronde (0 = geen): een begonnen beurt loopt door
	Prices  []Rate
	FeedIn  []Rate
	Solar   []Rate
}

// DHWTarget: het plan (sww.go), plus een begonnen beurt die doorloopt en het zonne-overschot van evcc.
// Planned = opwarmdoel van het plan (0 als het plan nu niet opwarmt), voor de volgende ronde.
func DHWTarget(c Config, s Schedule, l Learned, in DHWInput) DHWPlan {
	p := PlanDHW(c, s, l, in)
	if p.Target > s.DHWBase {
		p.Planned = p.Target
	}
	// een begonnen beurt loopt door tot de boiler warm is (geen aan/uit-gependel als het plan herschuift)
	if in.Prev > p.Target && in.Temp < in.Prev-1 {
		p.Target, p.Planned = in.Prev, in.Prev
		p.Why = fmt.Sprintf("opwarmen tot %.0f °C loopt door", in.Prev)
	}
	if in.PVBoost {
		boost, w := c.DHWBoost, "zonne-overschot (evcc)"
		if in.PVHot {
			boost, w = c.DHWBoostHot, "veel zonne-overschot of negatieve terugleverprijs (evcc): ook met de bijverwarming"
		}
		if boost > p.Target {
			p.Target, p.Why = boost, w
		}
	}
	return p
}

type HeatInput struct {
	Now    time.Time
	Tin    float64
	Tout   float64
	Prev   float64 // doel van de vorige ronde: een begonnen voorverwarming loopt door
	Prices []Rate
}

type HeatPlan struct {
	Target       float64 // 0 = verwarming uit (zomer)
	Why          string
	NextWindow   time.Time
	PreheatStart time.Time
	LeadMinutes  float64
}

// HeatTarget: gewenste kamertemperatuur. In een periode van het schema: die temperatuur. Daarbuiten: eco,
// behalve als het tijd is om voor te verwarmen (geleerde opwarmtijd), en dan het goedkoopste startmoment
// tot MaxEarlyMinutes vroeger.
func HeatTarget(c Config, s Schedule, l Learned, in HeatInput) HeatPlan {
	now := in.Now
	switch s.Mode {
	case ModeSummer:
		return HeatPlan{Why: "zomer: verwarming uit"}
	case ModeVacation:
		return HeatPlan{Target: s.Away, Why: fmt.Sprintf("vakantie: %.0f °C", s.Away)}
	}
	if p, ok := periodAt(s.Heat, now); ok {
		return HeatPlan{Target: p.Temp, Why: fmt.Sprintf("schema tot %s: %.0f °C", hhmm(p.To), p.Temp)}
	}
	plan := HeatPlan{Target: s.Eco, Why: fmt.Sprintf("eco %.0f °C", s.Eco)}
	ws, next, ok := nextPeriod(s.Heat, now, s.Eco)
	if !ok {
		return plan
	}
	plan.NextWindow = ws
	rise := next.Temp - math.Min(in.Tin, next.Temp)
	if rise < 0.5 {
		rise = next.Temp - s.Eco // kamer is nog warm: rekenen alsof hij tot eco afkoelt, voorzichtig
		rise = math.Max(0.5, rise/2)
	}
	lead := l.LeadMinutes(in.Tout, rise)
	plan.LeadMinutes = lead
	latest := ws.Add(-time.Duration(lead) * time.Minute)
	best, bestCost := latest, math.Inf(1)
	for e := 0; e <= c.MaxEarlyMinutes; e += 15 {
		st := latest.Add(-time.Duration(e) * time.Minute)
		p, ok := avgPrice(in.Prices, st, st.Add(time.Duration(lead)*time.Minute))
		if !ok {
			if e == 0 {
				bestCost = 0 // geen prijzen: gewoon op de laatste goede tijd
			}
			break
		}
		cost := p * (1 + c.EarlyPenaltyPerHour*float64(e)/60)
		if cost < bestCost-1e-9 {
			best, bestCost = st, cost
		}
	}
	plan.PreheatStart = best
	// begonnen voorverwarming loopt door tot het begin van de periode: naarmate de kamer warmer wordt, wordt de
	// berekende opwarmtijd korter en zou het startmoment anders telkens verschuiven (aan/uit-gependel)
	started := in.Prev == next.Temp && ws.Sub(now) <= time.Duration(lead+float64(c.MaxEarlyMinutes)+60)*time.Minute
	if !now.Before(best) || started {
		plan.Target = next.Temp
		plan.Why = fmt.Sprintf("voorverwarmen naar %.0f °C voor %s (±%.0f min nodig)", next.Temp, ws.Format("15:04"), lead)
	}
	return plan
}
