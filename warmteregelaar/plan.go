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

type Config struct {
	// sanitair warm water (de temperaturen per periode staan in het weekschema)
	DHWBoost                           float64
	NightFrom, NightTo                 int     // uur: venster voor de nachtbeurt
	NightFallbackFrom, NightFallbackTo int     // minuten: vaste nachtbeurt als er geen prijzen zijn
	SolarSkipKWh                       float64 // zoveel zon verwacht tussen 10 en 16 u → geen nachtbeurt
	PrepHours                          int     // zoveel uur voor een boilerperiode al bijwarmen in de goedkoopste kwartieren
	// verwarming
	MaxEarlyMinutes     int     // hoeveel vroeger dan nodig mag voorverwarmen starten als het goedkoper is
	EarlyPenaltyPerHour float64 // vroeger starten kost meer energie: zoveel % duurder per uur
}

func defaultConfig() Config {
	return Config{
		DHWBoost: 55, NightFrom: 0, NightTo: 7,
		NightFallbackFrom: 60, NightFallbackTo: 150, SolarSkipKWh: 12, PrepHours: 5,
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

// afkoeling om vooruit te rekenen: hoogstens 1 °C per uur (een geïsoleerde boiler zonder tappen koelt trager;
// een bad of douche is geen afkoeling en mag de voorspelling niet laten ontsporen)
func (l Learned) coolRate() float64 { return clamp(l.DHWCoolRate, 0, 1) }

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
func DHWTarget(c Config, s Schedule, l Learned, in DHWInput) (float64, string) {
	now := in.Now
	target, why := s.DHWBase, fmt.Sprintf("gewoon %.0f °C", s.DHWBase)
	set := func(t float64, w string) {
		if t > target {
			target, why = t, w
		}
	}
	if in.PVBoost {
		set(c.DHWBoost, "zonne-overschot (evcc)")
	}
	if s.Mode == ModeVacation {
		if target == s.DHWBase {
			why = fmt.Sprintf("vakantie: %.0f °C", s.DHWBase)
		}
		return target, why
	}
	if p, ok := periodAt(s.DHW, now); ok {
		set(p.Temp, fmt.Sprintf("schema %s–%s: minstens %.0f °C", hhmm(p.From), hhmm(p.To), p.Temp))
	}
	rate := math.Max(2, l.DHWHeatRate)
	h := now.Hour()

	// nachtbeurt in de goedkoopste kwartieren, tenzij er morgen genoeg zon is
	if h >= c.NightFrom && h < c.NightTo {
		day := atMinute(now, 0)
		sun := solarKWh(in.Solar, day.Add(10*time.Hour), day.Add(16*time.Hour))
		if sun < c.SolarSkipKWh {
			nf, nt := day.Add(time.Duration(c.NightFrom)*time.Hour), day.Add(time.Duration(c.NightTo)*time.Hour)
			if _, ok := avgPrice(in.Prices, nf, nt); ok {
				// vast aantal kwartieren (van ±de warmste periode van de dag tot de boost), zodat de keuze de hele
				// nacht dezelfde blijft; een begonnen beurt loopt door tot de boiler warm is
				ref := s.DHWBase
				for _, p := range s.DHW[dayIndex(now)] {
					ref = math.Max(ref, p.Temp)
				}
				need := int(clamp(math.Ceil((c.DHWBoost-ref+2)/rate*4), 2, 12))
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

	// voor een periode: zakt de boiler tegen het begin meer dan 2 °C onder de temperatuur van de periode (dan zou
	// de warmtepomp zelf bijwarmen, met zijn eigen marge van 2 °C), dan al bijwarmen tot die temperatuur in de
	// goedkoopste kwartieren ervoor. Niet warmer dan de periode vraagt.
	if ps, p, ok := nextPeriod(s.DHW, now, s.DHWBase); ok && ps.Sub(now) <= time.Duration(c.PrepHours)*time.Hour {
		if _, inside := periodAt(s.DHW, now); !inside || p.Temp > target {
			prep := p.Temp
			hours := ps.Sub(now).Hours()
			atStart := in.Temp - l.coolRate()*hours
			if atStart < p.Temp-2 {
				need := int(clamp(math.Ceil((prep-atStart)/rate*4), 1, 16))
				left := int(math.Ceil(ps.Sub(now.Truncate(15*time.Minute)).Minutes() / 15))
				if left <= need || containsQuarter(cheapestQuarters(in.Prices, now, ps, need), now) {
					set(prep, fmt.Sprintf("bijwarmen voor %s (dan verwacht %.0f °C)", ps.Format("15:04"), atStart))
				}
			}
			if in.Prev == prep && in.Temp < prep-1 {
				set(prep, fmt.Sprintf("bijwarmen voor %s loopt door", ps.Format("15:04")))
			}
		}
	}
	return target, why
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
