package main

// Beslissingen van de warmteregelaar: welke boiler- en kamertemperatuur moet de warmtepomp nu krijgen.
// Alles hier is zuiver rekenwerk (geen netwerk), zodat het met tests na te rekenen is.

import (
	"fmt"
	"math"
	"sort"
	"time"
)

type Rate struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Value float64   `json:"value"`
}

type Window struct{ From, To int } // minuten na middernacht

type Config struct {
	// sanitair warm water
	DHWBase, DHWEvening, DHWBoost, DHWPrep float64
	EveningFrom, EveningTo                 int     // uur
	NightFrom, NightTo                     int     // uur: venster voor de nachtbeurt
	NightFallbackFrom, NightFallbackTo     int     // minuten: vaste nachtbeurt als er geen prijzen zijn
	SolarSkipKWh                           float64 // zoveel zon verwacht tussen 10 en 16 u → geen nachtbeurt
	// verwarming
	Comfort, Eco        float64
	WeekdayWindows      []Window
	WeekendWindows      []Window
	MaxEarlyMinutes     int     // hoeveel vroeger dan nodig mag voorverwarmen starten als het goedkoper is
	EarlyPenaltyPerHour float64 // vroeger starten kost meer energie: zoveel % duurder per uur
}

func defaultConfig() Config {
	return Config{
		DHWBase: 40, DHWEvening: 48, DHWBoost: 55, DHWPrep: 52,
		EveningFrom: 17, EveningTo: 22, NightFrom: 0, NightTo: 7,
		NightFallbackFrom: 60, NightFallbackTo: 150, SolarSkipKWh: 12,
		Comfort: 22, Eco: 20,
		WeekdayWindows:  []Window{{6 * 60, 7*60 + 30}, {16 * 60, 22 * 60}},
		WeekendWindows:  []Window{{7 * 60, 22 * 60}},
		MaxEarlyMinutes: 60, EarlyPenaltyPerHour: 0.15,
	}
}

// Wat de regelaar geleerd heeft over de boiler en het huis.
type Learned struct {
	DHWHeatRate float64         `json:"dhwHeatRate"` // °C per uur tijdens een boilerbeurt
	DHWCoolRate float64         `json:"dhwCoolRate"` // °C per uur afkoeling + verbruik zonder beurt
	Preheat     []PreheatSample `json:"preheat"`     // hoe lang opwarmen duurde, bij welke buitentemperatuur
}

type PreheatSample struct {
	Time    time.Time `json:"time"`
	Tout    float64   `json:"tout"`
	Rise    float64   `json:"rise"` // °C die moest worden bijgewarmd
	Minutes float64   `json:"minutes"`
}

func defaultLearned() Learned { return Learned{DHWHeatRate: 8, DHWCoolRate: 0.4} }

// LeadMinutes: hoeveel minuten voor het venster de verwarming hoger moet om op tijd warm te zijn.
// Zonder metingen een voorzichtige schatting; met ≥ 4 metingen een rechte door de metingen
// (minuten per °C bijwarmen, afhankelijk van de buitentemperatuur).
func (l Learned) LeadMinutes(tout, rise float64) float64 {
	if rise <= 0 {
		return 0
	}
	cold := math.Max(0, 15-tout)
	if len(l.Preheat) < 4 {
		return clamp((30+4*cold)*rise, 45, 240)
	}
	// minuten per °C = a + b·cold, kleinste kwadraten
	var n, sx, sy, sxx, sxy float64
	for _, s := range l.Preheat {
		if s.Rise <= 0 {
			continue
		}
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

func (c Config) windowsFor(day time.Time) []Window {
	if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return c.WeekendWindows
	}
	return c.WeekdayWindows
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

// cheapestQuarters: de n goedkoopste kwartieren (begintijden) in [from, to).
func cheapestQuarters(rates []Rate, from, to time.Time, n int) []time.Time {
	type q struct {
		t time.Time
		p float64
	}
	var qs []q
	start := from.Truncate(15 * time.Minute)
	for t := start; t.Before(to); t = t.Add(15 * time.Minute) {
		if p, ok := priceAt(rates, t); ok {
			qs = append(qs, q{t, p})
		}
	}
	sort.SliceStable(qs, func(i, j int) bool { return qs[i].p < qs[j].p })
	if n > len(qs) {
		n = len(qs)
	}
	res := make([]time.Time, n)
	for i := 0; i < n; i++ {
		res[i] = qs[i].t
	}
	return res
}

func containsQuarter(qs []time.Time, now time.Time) bool {
	cur := now.Truncate(15 * time.Minute)
	for _, t := range qs {
		if t.Equal(cur) {
			return true
		}
	}
	return false
}

// solarKWh: verwachte zonne-opbrengst (kWh) tussen from en to; solar-rates zijn in W.
func solarKWh(rates []Rate, from, to time.Time) float64 {
	var kwh float64
	for _, r := range rates {
		s, e := r.Start, r.End
		if s.Before(from) {
			s = from
		}
		if e.After(to) {
			e = to
		}
		if e.After(s) {
			kwh += r.Value * e.Sub(s).Hours() / 1000
		}
	}
	return kwh
}

type DHWInput struct {
	Now     time.Time
	Temp    float64
	PVBoost bool    // evcc: zonne-overschot voor de boiler
	Prev    float64 // doel van de vorige ronde: een begonnen beurt loopt door tot de boiler warm is
	Prices  []Rate
	Solar   []Rate
}

// DHWTarget: gewenste boilertemperatuur en waarom.
func DHWTarget(c Config, l Learned, in DHWInput) (float64, string) {
	now := in.Now
	target, why := c.DHWBase, fmt.Sprintf("gewoon %.0f °C", c.DHWBase)
	set := func(t float64, w string) {
		if t > target {
			target, why = t, w
		}
	}
	h := now.Hour()
	if h >= c.EveningFrom && h < c.EveningTo {
		set(c.DHWEvening, fmt.Sprintf("avond (%d–%d u): minstens %.0f °C", c.EveningFrom, c.EveningTo, c.DHWEvening))
	}
	if in.PVBoost {
		set(c.DHWBoost, "zonne-overschot (evcc)")
	}
	rate := math.Max(2, l.DHWHeatRate)

	// nachtbeurt in de goedkoopste kwartieren, tenzij er morgen genoeg zon is
	if h >= c.NightFrom && h < c.NightTo {
		day := atMinute(now, 0)
		sun := solarKWh(in.Solar, day.Add(10*time.Hour), day.Add(16*time.Hour))
		if sun < c.SolarSkipKWh {
			nf, nt := day.Add(time.Duration(c.NightFrom)*time.Hour), day.Add(time.Duration(c.NightTo)*time.Hour)
			if _, ok := avgPrice(in.Prices, nf, nt); ok {
				// vast aantal kwartieren (van ±avondtemperatuur tot de boost), zodat de keuze de hele nacht
				// dezelfde blijft; een begonnen beurt loopt door tot de boiler warm is
				need := int(clamp(math.Ceil((c.DHWBoost-c.DHWEvening+2)/rate*4), 2, 12))
				if containsQuarter(cheapestQuarters(in.Prices, nf, nt, need), now) {
					set(c.DHWBoost, fmt.Sprintf("nachtbeurt in de goedkoopste kwartieren (%.0f kWh zon verwacht)", sun))
				} else if in.Prev == c.DHWBoost && in.Temp < c.DHWBoost-1 {
					set(c.DHWBoost, "nachtbeurt loopt door tot de boiler warm is")
				}
			} else if m := now.Hour()*60 + now.Minute(); m >= c.NightFallbackFrom && m < c.NightFallbackTo {
				set(c.DHWBoost, "nachtbeurt (vast uur, geen prijzen)")
			}
		}
	}

	// voor de avond: komt de boiler om 17 u niet aan 48 °C, dan bijwarmen in de goedkoopste kwartieren ervoor
	ev := atMinute(now, c.EveningFrom*60)
	if h >= 12 && now.Before(ev) {
		hours := ev.Sub(now).Hours()
		at17 := in.Temp - math.Max(0, l.DHWCoolRate)*hours
		if at17 < c.DHWEvening+1 {
			need := int(math.Ceil((c.DHWPrep - at17) / rate * 4))
			need = int(clamp(float64(need), 1, 16))
			left := int(math.Ceil(ev.Sub(now.Truncate(15*time.Minute)).Minutes() / 15))
			if left <= need || containsQuarter(cheapestQuarters(in.Prices, now, ev, need), now) {
				set(c.DHWPrep, fmt.Sprintf("bijwarmen voor de avond (om %d u verwacht %.0f °C)", c.EveningFrom, at17))
			}
		}
		if in.Prev == c.DHWPrep && in.Temp < c.DHWPrep-1 {
			set(c.DHWPrep, "bijwarmen voor de avond loopt door")
		}
	}
	return target, why
}

type HeatInput struct {
	Now    time.Time
	Tin    float64
	Tout   float64
	Prices []Rate
}

type HeatPlan struct {
	Target       float64
	Why          string
	NextWindow   time.Time
	PreheatStart time.Time
	LeadMinutes  float64
}

// HeatTarget: gewenste kamertemperatuur. In een venster: comfort. Daarbuiten: eco, behalve als het tijd is
// om voor te verwarmen (geleerde opwarmtijd), en dan het goedkoopste startmoment tot MaxEarlyMinutes vroeger.
func HeatTarget(c Config, l Learned, in HeatInput) HeatPlan {
	now := in.Now
	m := now.Hour()*60 + now.Minute()
	for _, w := range c.windowsFor(now) {
		if m >= w.From && m < w.To {
			return HeatPlan{Target: c.Comfort, Why: fmt.Sprintf("comfortvenster tot %s", atMinute(now, w.To).Format("15:04"))}
		}
	}
	// volgende venster (vandaag of morgen)
	var ws time.Time
	for d := 0; d < 3 && ws.IsZero(); d++ {
		day := atMinute(now, 0).AddDate(0, 0, d)
		for _, w := range c.windowsFor(day) {
			if t := atMinute(day, w.From); t.After(now) {
				ws = t
				break
			}
		}
	}
	plan := HeatPlan{Target: c.Eco, Why: fmt.Sprintf("eco %.0f °C", c.Eco), NextWindow: ws}
	if ws.IsZero() {
		return plan
	}
	rise := c.Comfort - math.Min(in.Tin, c.Comfort)
	if rise < 0.5 {
		rise = c.Comfort - c.Eco // kamer is nog warm: rekenen alsof hij tot eco afkoelt, voorzichtig
		rise = math.Max(0.5, rise/2)
	}
	lead := l.LeadMinutes(in.Tout, rise)
	plan.LeadMinutes = lead
	latest := ws.Add(-time.Duration(lead) * time.Minute)
	best, bestCost := latest, math.Inf(1)
	for s := 0; s <= c.MaxEarlyMinutes; s += 15 {
		st := latest.Add(-time.Duration(s) * time.Minute)
		p, ok := avgPrice(in.Prices, st, st.Add(time.Duration(lead)*time.Minute))
		if !ok {
			if s == 0 {
				bestCost = 0 // geen prijzen: gewoon op de laatste goede tijd
			}
			break
		}
		cost := p * (1 + c.EarlyPenaltyPerHour*float64(s)/60)
		if cost < bestCost-1e-9 {
			best, bestCost = st, cost
		}
	}
	plan.PreheatStart = best
	if !now.Before(best) {
		plan.Target = c.Comfort
		plan.Why = fmt.Sprintf("voorverwarmen voor %s (±%.0f min nodig)", ws.Format("15:04"), lead)
	}
	return plan
}
