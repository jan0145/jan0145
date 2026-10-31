package main

// Voorspelling en geschiedenis voor de grafieken in de Stroom-app:
//   kamertemperatuur + ingestelde kamertemperatuur, boilertemperatuur + ingestelde boilertemperatuur.
// De voorspelling wordt elk uur bewaard, zodat de app later kan tonen wat er voorspeld was naast wat er gebeurde.

import (
	"bufio"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Forecast struct {
	Start   time.Time `json:"start"` // begin van het eerste kwartier
	Room    []float64 `json:"room"`  // kamertemperatuur aan het einde van elk kwartier
	RoomSet []float64 `json:"roomSet"`
	DHW     []float64 `json:"dhw"`
	DHWSet  []float64 `json:"dhwSet"`
}

// afkoeling van het huis: °C per uur per °C verschil met buiten
func (l Learned) roomLoss() float64 {
	if l.RoomLoss <= 0 {
		return 0.01
	}
	return clamp(l.RoomLoss, 0.002, 0.05)
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// forecastRoom: ingestelde temperatuur volgens het schema (met voorverwarmen) en de kamertemperatuur:
// opwarmen met de geleerde snelheid tot de ingestelde temperatuur, anders afkoelen naar buiten toe
// (de warmtepomp houdt de ingestelde temperatuur vast).
func forecastRoom(c Config, s Schedule, l Learned, now time.Time, tin, tout float64, prev float64, prices []Rate) ([]float64, []float64) {
	start := now.Truncate(quarter)
	room, set := make([]float64, dhwHorizon), make([]float64, dhwHorizon)
	t := tin
	for i := 0; i < dhwHorizon; i++ {
		qt := start.Add(time.Duration(i) * quarter)
		at := qt
		if i == 0 {
			at = now
		}
		p := HeatTarget(c, s, l, HeatInput{Now: at, Tin: t, Tout: tout, Prev: prev, Prices: prices})
		prev = p.Target
		sp := p.Target
		if sp == 0 { // zomer: verwarming uit
			sp = math.NaN()
		}
		if !math.IsNaN(sp) && sp > t+0.05 {
			rate := 60 / math.Max(10, l.LeadMinutes(tout, 1)) // °C per uur
			t = math.Min(sp, t+rate/4)
		} else {
			cool := l.roomLoss() * math.Max(0, t-tout) / 4
			t -= cool
			if !math.IsNaN(sp) {
				t = math.Max(t, math.Min(sp, t+cool)) // niet onder de ingestelde temperatuur
			}
		}
		room[i] = round1(t)
		if math.IsNaN(sp) {
			set[i] = 0
		} else {
			set[i] = sp
		}
	}
	return room, set
}

// ingestelde boilertemperatuur per kwartier volgens het plan
func dhwSetpoints(s Schedule, p DHWPlan) []float64 {
	set := make([]float64, len(p.Forecast))
	heat := map[time.Time]bool{}
	for _, h := range p.Heat {
		heat[h] = true
	}
	for i := range set {
		qt := p.Start.Add(time.Duration(i) * quarter)
		if !heat[qt] {
			set[i] = s.DHWBase
			continue
		}
		// doel = temperatuur aan het einde van dit opwarmblok
		j := i
		for j+1 < len(set) && heat[qt.Add(time.Duration(j+1-i)*quarter)] {
			j++
		}
		set[i] = math.Min(dhwMaxHP, math.Ceil(p.Forecast[j]))
	}
	return set
}

// learnRoomLoss: tijdens eco (ingesteld duidelijk onder de kamertemperatuur) koelt het huis vrij af
func learnRoomLoss(now time.Time, tin, tout, set float64) {
	m := st.RoomMark
	cooling := set > 0 && set < tin-0.5
	if m == nil || !cooling {
		if cooling {
			st.RoomMark = &roomMark{Time: now, Tin: tin, Tout: tout}
		} else {
			st.RoomMark = nil
		}
		return
	}
	h := now.Sub(m.Time).Hours()
	if h < 2 {
		return
	}
	dt := (m.Tin+tin)/2 - (m.Tout+tout)/2
	if drop := m.Tin - tin; dt > 3 && drop >= 0 {
		k := drop / h / dt
		if st.Learned.RoomLoss <= 0 {
			st.Learned.RoomLoss = 0.01
		}
		st.Learned.RoomLoss = clamp(0.8*st.Learned.RoomLoss+0.2*k, 0.002, 0.05)
	}
	st.RoomMark = &roomMark{Time: now, Tin: tin, Tout: tout}
}

type roomMark struct {
	Time      time.Time `json:"time"`
	Tin, Tout float64
}

// elk uur de voorspelling bewaren (ruim 2 dagen)
func keepForecast(f Forecast) {
	n := len(st.Forecasts)
	if n > 0 && f.Start.Sub(st.Forecasts[n-1].Start) < time.Hour {
		return
	}
	st.Forecasts = append(st.Forecasts, f)
	if n+1 > 54 {
		st.Forecasts = st.Forecasts[n+1-54:]
	}
}

// voorspelling die ±24 uur geleden gemaakt werd
func forecastFrom(ago time.Time) *Forecast {
	var best *Forecast
	for i := range st.Forecasts {
		f := &st.Forecasts[i]
		if !f.Start.After(ago.Add(30 * time.Minute)) {
			best = f
		}
	}
	return best
}

type histRow struct {
	T       time.Time `json:"t"`
	Tout    float64   `json:"tout"`
	Room    float64   `json:"room"`
	RoomSet float64   `json:"roomSet"`
	DHW     float64   `json:"dhw"`
	DHWSet  float64   `json:"dhwSet"`
	// zelf ingesteld (bediening of app) in plaats van de regelaar
	RoomManual bool `json:"roomManual,omitempty"`
	DHWManual  bool `json:"dhwManual,omitempty"`
	// kamertemperatuur zoals de Shelly ze meet (0 = geen meting)
	Shelly float64 `json:"shelly,omitempty"`
}

// aggregateHistory: gemiddelde per tijdstap (gemeten temperaturen); de ingestelde temperatuur is de hoogste in die stap
// (zo blijven de opwarmperiodes zichtbaar), "zelf ingesteld" als dat er ergens in de stap was
func aggregateHistory(rows []histRow, step time.Duration) []histRow {
	var out []histRow
	var acc histRow
	var n, nSh int
	flush := func() {
		if n == 0 {
			return
		}
		f := float64(n)
		acc.Tout, acc.Room, acc.DHW = round1(acc.Tout/f), round1(acc.Room/f), round1(acc.DHW/f)
		if nSh > 0 {
			acc.Shelly = round1(acc.Shelly / float64(nSh))
		}
		out = append(out, acc)
	}
	for _, r := range rows {
		b := r.T.Truncate(step)
		if n == 0 || !b.Equal(acc.T) {
			flush()
			acc, n, nSh = histRow{T: b}, 0, 0
		}
		n++
		acc.Tout += r.Tout
		acc.Room += r.Room
		acc.DHW += r.DHW
		if r.Shelly > 0 {
			acc.Shelly += r.Shelly
			nSh++
		}
		acc.RoomSet, acc.DHWSet = math.Max(acc.RoomSet, r.RoomSet), math.Max(acc.DHWSet, r.DHWSet)
		acc.RoomManual, acc.DHWManual = acc.RoomManual || r.RoomManual, acc.DHWManual || r.DHWManual
	}
	flush()
	return out
}

// geschiedenis uit de maandbestanden warmte-JJJJ-MM.csv (elke 5 min een regel)
func readHistory(from, to time.Time) []histRow {
	var rows []histRow
	for m := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.Local); !m.After(to); m = m.AddDate(0, 1, 0) {
		f, err := os.Open(filepath.Join(dataDir, "warmte-"+m.Format("2006-01")+".csv"))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			c := strings.Split(sc.Text(), ";")
			if len(c) < 7 {
				continue
			}
			t, err := time.ParseInLocation("2006-01-02 15:04", c[0], time.Local)
			if err != nil || t.Before(from) || t.After(to) {
				continue
			}
			num := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
			row := histRow{T: t, Tout: num(c[1]), Room: num(c[2]), RoomSet: num(c[3]), DHW: num(c[5]), DHWSet: num(c[6])}
			if len(c) >= 15 { // oudere regels hebben deze kolommen niet
				row.RoomManual, row.DHWManual = c[13] == "1", c[14] == "1"
			}
			if len(c) >= 16 && c[15] != "" {
				row.Shelly = num(c[15])
			}
			rows = append(rows, row)
		}
		f.Close()
	}
	return rows
}

func serveForecast(h *http.ServeMux) {
	// /grafiek: geschiedenis (24 u), de huidige voorspelling (24 u) en de voorspelling van 24 u geleden.
	// /grafiek?from=…&to=…&step=… (unix-seconden): geschiedenis van een periode, gemiddeld per step seconden
	// (dag, week, maand in de app); de voorspelling alleen als de periode nog niet voorbij is.
	h.HandleFunc("/grafiek", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		now := time.Now()
		from, to := now.Add(-24*time.Hour), now
		q := r.URL.Query()
		period := q.Get("from") != ""
		if period {
			f, _ := strconv.ParseInt(q.Get("from"), 10, 64)
			t, _ := strconv.ParseInt(q.Get("to"), 10, 64)
			from, to = time.Unix(f, 0), time.Unix(t, 0)
			if !to.After(from) || to.Sub(from) > 45*24*time.Hour {
				http.Error(w, "ongeldige periode", http.StatusBadRequest)
				return
			}
		}
		hist := readHistory(from, to)
		if step, _ := strconv.Atoi(q.Get("step")); step > 300 {
			hist = aggregateHistory(hist, time.Duration(step)*time.Second)
		}
		mu.Lock()
		var cur, old *Forecast
		if period {
			if to.After(now) && st.Current.Start.After(now.Add(-time.Hour)) {
				c := st.Current
				cur = &c
			}
		} else if n := len(st.Forecasts); n > 0 {
			c := st.Current
			cur = &c
			old = forecastFrom(now.Add(-24 * time.Hour))
		} else if st.Current.Start.After(now.Add(-time.Hour)) {
			c := st.Current
			cur = &c
		}
		// de voorspelde kamertemperatuur staat op de schaal van de warmtepomp: min dit verschil = Shelly
		var off *float64
		if _, ok := shellyNow(now); ok {
			o := math.Round(st.RoomOffset*100) / 100
			off = &o
		}
		b, _ := json.Marshal(struct {
			History  []histRow `json:"history"`
			Forecast *Forecast `json:"forecast"`
			Past     *Forecast `json:"past"`             // voorspelling van ±24 u geleden
			Offset   *float64  `json:"offset,omitempty"` // warmtepompsensor − Shelly
		}{hist, cur, old, off})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	})
}
