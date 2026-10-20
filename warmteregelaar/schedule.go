package main

// Weekschema van verwarming en boiler, aan te passen in de Stroom-app (GET/POST /schedule).
// Per dag (0 = maandag) een lijst periodes met een temperatuur; buiten de periodes geldt eco (kamer)
// of de gewone boilertemperatuur.
//
// Stand:
//   normaal  – het schema
//   vakantie – schema uit: kamer op de vakantietemperatuur, boiler gewoon, geen nachtbeurt
//   zomer    – verwarming uit (warmtepomp op "alleen sanitair water"), boiler volgens het schema

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

type Period struct {
	From, To int // minuten na middernacht (To mag 1440 zijn)
	Temp     float64
}

type Schedule struct {
	Mode    string      `json:"mode"`
	Eco     float64     `json:"eco"`     // kamer buiten de periodes
	DHWBase float64     `json:"dhwBase"` // boiler buiten de periodes
	Away    float64     `json:"away"`    // kamer tijdens vakantie
	Heat    [7][]Period `json:"heat"`
	DHW     [7][]Period `json:"dhw"`
}

const (
	ModeNormal   = "normaal"
	ModeVacation = "vakantie"
	ModeSummer   = "zomer"
)

// standaard: het schema van de strategie (weekdag 06–07:30 en 16–22 u, weekend 07–22 u; boiler 17–22 u ≥ 48 °C)
func defaultSchedule(comfort, eco, base, evening float64) Schedule {
	s := Schedule{Mode: ModeNormal, Eco: eco, DHWBase: base, Away: 16}
	for d := 0; d < 7; d++ {
		if d < 5 {
			s.Heat[d] = []Period{{6 * 60, 7*60 + 30, comfort}, {16 * 60, 22 * 60, comfort}}
		} else {
			s.Heat[d] = []Period{{7 * 60, 22 * 60, comfort}}
		}
		s.DHW[d] = []Period{{17 * 60, 22 * 60, evening}}
	}
	return s
}

func dayIndex(t time.Time) int { return (int(t.Weekday()) + 6) % 7 }

func hhmm(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

func parseHHMM(s string) (int, error) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || h < 0 || h > 24 || m < 0 || m > 59 || h*60+m > 1440 {
		return 0, fmt.Errorf("ongeldig uur %q", s)
	}
	return h*60 + m, nil
}

func (p Period) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		From string  `json:"from"`
		To   string  `json:"to"`
		Temp float64 `json:"temp"`
	}{hhmm(p.From), hhmm(p.To), p.Temp})
}

func (p *Period) UnmarshalJSON(b []byte) error {
	var x struct {
		From, To string
		Temp     float64
	}
	if err := json.Unmarshal(b, &x); err != nil {
		return err
	}
	var err error
	if p.From, err = parseHHMM(x.From); err != nil {
		return err
	}
	if p.To, err = parseHHMM(x.To); err != nil {
		return err
	}
	p.Temp = x.Temp
	return nil
}

// Validate: zinnige temperaturen, periodes gesorteerd en zonder overlap.
func (s *Schedule) Validate() error {
	switch s.Mode {
	case ModeNormal, ModeVacation, ModeSummer:
	case "":
		s.Mode = ModeNormal
	default:
		return fmt.Errorf("onbekende stand %q", s.Mode)
	}
	if s.Eco < 10 || s.Eco > 25 || s.Away < 5 || s.Away > 25 || s.DHWBase < 30 || s.DHWBase > 60 {
		return fmt.Errorf("eco 10–25 °C, vakantie 5–25 °C, boiler 30–60 °C")
	}
	check := func(name string, days *[7][]Period, lo, hi float64) error {
		for d := range days {
			ps := days[d]
			sort.Slice(ps, func(i, j int) bool { return ps[i].From < ps[j].From })
			if len(ps) > 6 {
				return fmt.Errorf("%s: hoogstens 6 periodes per dag", name)
			}
			for i, p := range ps {
				if p.To <= p.From {
					return fmt.Errorf("%s: periode %s–%s eindigt voor ze begint", name, hhmm(p.From), hhmm(p.To))
				}
				if p.Temp < lo || p.Temp > hi {
					return fmt.Errorf("%s: temperatuur %.0f °C buiten %.0f–%.0f °C", name, p.Temp, lo, hi)
				}
				if i > 0 && p.From < ps[i-1].To {
					return fmt.Errorf("%s: periodes %s en %s overlappen", name, hhmm(ps[i-1].From), hhmm(p.From))
				}
			}
		}
		return nil
	}
	if err := check("verwarming", &s.Heat, 10, 26); err != nil {
		return err
	}
	return check("boiler", &s.DHW, 30, 60)
}

// periode waarin t valt (ok=false: geen)
func periodAt(days [7][]Period, t time.Time) (Period, bool) {
	m := t.Hour()*60 + t.Minute()
	for _, p := range days[dayIndex(t)] {
		if m >= p.From && m < p.To {
			return p, true
		}
	}
	return Period{}, false
}

// volgende periode die na t begint en warmer is dan min (binnen 3 dagen); geeft begin en periode
func nextPeriod(days [7][]Period, t time.Time, min float64) (time.Time, Period, bool) {
	for d := 0; d < 4; d++ {
		day := atMinute(t, 0).AddDate(0, 0, d)
		for _, p := range days[dayIndex(day)] {
			if s := atMinute(day, p.From); s.After(t) && p.Temp > min {
				return s, p, true
			}
		}
	}
	return time.Time{}, Period{}, false
}
