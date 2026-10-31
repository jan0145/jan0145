package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var bru, _ = time.LoadLocation("Europe/Brussels")

// maandag 2 november 2026
func at(h, m int) time.Time { return time.Date(2026, 11, 2, h, m, 0, 0, bru) }

// prijzen per kwartier voor de hele dag: overal 0.30, behalve de opgegeven kwartieren
func prices(cheap map[time.Time]float64) []Rate {
	var rs []Rate
	for t := at(0, 0); t.Before(at(0, 0).AddDate(0, 0, 2)); t = t.Add(15 * time.Minute) {
		p := 0.30
		if v, ok := cheap[t]; ok {
			p = v
		}
		rs = append(rs, Rate{t, t.Add(15 * time.Minute), p})
	}
	return rs
}

// in elke periode minstens de eis (behalve als het echt niet haalbaar is)
func assertFeasible(t *testing.T, s Schedule, p DHWPlan, what string) {
	t.Helper()
	for i, v := range p.Forecast {
		start := p.Start
		req, _ := dhwRequirement(s, start.Add(time.Duration(i)*quarter))
		if v < req-1e-6 {
			t.Fatalf("%s: om %s verwacht %.1f °C < %.0f °C (%s)", what, start.Add(time.Duration(i+1)*quarter).Format("Mon 15:04"), v, req, p.Why)
		}
	}
}

func flat(p float64) []Rate {
	var rs []Rate
	for t := at(0, 0); t.Before(at(0, 0).AddDate(0, 0, 3)); t = t.Add(15 * time.Minute) {
		rs = append(rs, Rate{t, t.Add(15 * time.Minute), p})
	}
	return rs
}

func TestDHWPlanBasics(t *testing.T) {
	c, s, l := defaultConfig(), defaultSchedule(22, 20, 40, 48), defaultLearned()
	// 11:00, 45 °C: voor de avond (17–22 u ≥ 46 °C, douches 19–22 u) moet hij ergens opwarmen, maar niet nu
	p := DHWTarget(c, s, l, DHWInput{Now: at(11, 0), Temp: 45, Prices: flat(0.30)})
	if p.Target != 40 || len(p.Heat) == 0 || !strings.Contains(p.Why, "volgende opwarming") {
		t.Fatalf("overdag: %+v", p.Why)
	}
	assertFeasible(t, s, p, "overdag")
	// genoeg warm water: niets doen
	p = DHWTarget(c, s, l, DHWInput{Now: at(15, 0), Temp: 55, Prices: flat(0.30)})
	if p.Target != 40 || len(p.Heat) != 0 {
		t.Fatalf("warm genoeg: %v %s", p.Heat, p.Why)
	}
	// zonne-overschot van evcc: 55, met veel overschot 60
	if p := DHWTarget(c, s, l, DHWInput{Now: at(11, 0), Temp: 45, PVBoost: true}); p.Target != 55 || !strings.Contains(p.Why, "zon") {
		t.Fatalf("zonne-overschot: %v %s", p.Target, p.Why)
	}
	if p := DHWTarget(c, s, l, DHWInput{Now: at(11, 0), Temp: 45, PVBoost: true, PVHot: true}); p.Target != 60 {
		t.Fatalf("veel overschot: %v %s", p.Target, p.Why)
	}
	// een begonnen beurt loopt door
	if p := DHWTarget(c, s, l, DHWInput{Now: at(11, 0), Temp: 49, Prev: 52, Prices: flat(0.30)}); p.Target != 52 || !strings.Contains(p.Why, "loopt door") {
		t.Fatalf("doorlopen: %v %s", p.Target, p.Why)
	}
}

func TestDHWPlanNightVsDay(t *testing.T) {
	c, s, l := defaultConfig(), defaultSchedule(22, 20, 40, 48), defaultLearned()
	// goedkoop 02:00–05:00, overdag duur: 's nachts warm genoeg maken voor de hele dag
	cheap := map[time.Time]float64{}
	for q := at(2, 0); q.Before(at(5, 0)); q = q.Add(15 * time.Minute) {
		cheap[q] = 0.10
	}
	pr := prices(cheap)
	p := DHWTarget(c, s, l, DHWInput{Now: at(1, 0), Temp: 44, Prices: pr})
	assertFeasible(t, s, p, "nacht")
	// tot 55 °C in de goedkope uren; wat dan nog ontbreekt (warmteverlies over de dag), pas 's avonds
	night := 0
	for _, h := range p.Heat {
		if !h.Before(at(2, 0)) && h.Before(at(5, 0)) {
			night++
		} else if h.Before(at(17, 0)) {
			t.Fatalf("overdag opwarmen terwijl het 's nachts goedkoper was: %v (%s)", p.Heat, p.Why)
		}
	}
	if night < len(p.Heat)*2/3 {
		t.Fatalf("te weinig 's nachts: %v (%s)", p.Heat, p.Why)
	}
	// op het geplande moment echt opwarmen, tot ruim boven 48 °C (de hele dag moet ermee toe)
	first := p.Heat[0]
	if q := DHWTarget(c, s, l, DHWInput{Now: first.Add(5 * time.Minute), Temp: 44, Prices: pr}); q.Target < 50 {
		t.Fatalf("om %s opwarmen: %v %s", first.Format("15:04"), q.Target, q.Why)
	}
	// zon verwacht 12–15 u en een lage terugleverprijs: dan liever overdag
	sun := []Rate{{at(12, 0), at(15, 0), 4000}}
	feed := flat(0.03)
	p = DHWTarget(c, s, l, DHWInput{Now: at(1, 0), Temp: 46, Prices: flat(0.30), Solar: sun, FeedIn: feed})
	assertFeasible(t, s, p, "zon")
	for _, h := range p.Heat {
		if h.Before(at(12, 0)) || !h.Before(at(15, 0)) && h.Before(at(22, 0)) {
			t.Fatalf("zon: opwarmen buiten 12–15 u: %v (%s)", p.Heat, p.Why)
		}
	}
	// weekend: meer douches 's avonds → warmer opwarmen dan op een weekdag
	sat := func(h, m int) time.Time { return time.Date(2026, 11, 7, h, m, 0, 0, bru) }
	wd := DHWTarget(c, s, l, DHWInput{Now: at(16, 0), Temp: 44, Prices: flat(0.30)})
	we := DHWTarget(c, s, l, DHWInput{Now: sat(16, 0), Temp: 44, Prices: flat(0.30)})
	if len(we.Heat) <= len(wd.Heat) {
		t.Fatalf("weekend %d kwartieren, weekdag %d", len(we.Heat), len(wd.Heat))
	}
}

func TestDHWLearnedUse(t *testing.T) {
	c, s, l := defaultConfig(), defaultSchedule(22, 20, 40, 48), defaultLearned()
	// geleerd: dinsdagavond (dag 1) een bad om 20:00 dat de boiler 12 °C doet zakken
	u := defaultDHWUse()
	u[1][80] = 12
	l.DHWUse = &u
	tue := func(h, m int) time.Time { return time.Date(2026, 11, 3, h, m, 0, 0, bru) }
	p := DHWTarget(c, s, l, DHWInput{Now: tue(12, 0), Temp: 50, Prices: flat(0.30)})
	// vlak voor het bad zo warm als de warmtepomp kan (55 °C), en meteen erna weer opwarmen
	i := int(tue(19, 45).Sub(p.Start) / quarter)
	if p.Forecast[i] < 54 {
		t.Fatalf("bad: om 20:00 maar %.1f °C (%s)", p.Forecast[i], p.Why)
	}
	if len(p.Heat) > 16 {
		t.Fatalf("bad: nutteloos veel opwarmen: %d kwartieren", len(p.Heat))
	}
}

func TestHeat(t *testing.T) {
	c, s, l := defaultConfig(), defaultSchedule(22, 20, 40, 48), defaultLearned()
	if p := HeatTarget(c, s, l, HeatInput{Now: at(6, 30), Tin: 21, Tout: 5}); p.Target != 22 {
		t.Fatalf("ochtendvenster: %+v", p)
	}
	if p := HeatTarget(c, s, l, HeatInput{Now: at(10, 0), Tin: 21, Tout: 5}); p.Target != 20 {
		t.Fatalf("overdag eco: %+v", p)
	}
	// 2 °C bijwarmen bij 5 °C buiten: (30+4·10)·2 = 140 min → start 13:40 voor het venster van 16:00
	p := HeatTarget(c, s, l, HeatInput{Now: at(13, 0), Tin: 20, Tout: 5})
	if p.Target != 20 || p.LeadMinutes != 140 || !p.PreheatStart.Equal(at(13, 40)) {
		t.Fatalf("nog geen voorverwarming: %+v", p)
	}
	if p := HeatTarget(c, s, l, HeatInput{Now: at(13, 45), Tin: 20, Tout: 5}); p.Target != 22 {
		t.Fatalf("voorverwarmen: %+v", p)
	}
	// begonnen voorverwarming loopt door, ook als de kamer al warmer is en de opwarmtijd dus korter
	if p := HeatTarget(c, s, l, HeatInput{Now: at(14, 30), Tin: 21.5, Tout: 5, Prev: 22}); p.Target != 22 || !strings.Contains(p.Why, "voorverwarmen") {
		t.Fatalf("voorverwarmen loopt door: %+v", p)
	}
	// maar niet uren op voorhand (na het ochtendvenster)
	if p := HeatTarget(c, s, l, HeatInput{Now: at(7, 35), Tin: 22, Tout: 5, Prev: 22}); p.Target != 20 {
		t.Fatalf("na het venster: %+v", p)
	}
	// goedkoper 30 min vroeger → vroeger starten
	cheap := map[time.Time]float64{}
	for t0 := at(13, 0); t0.Before(at(15, 30)); t0 = t0.Add(15 * time.Minute) {
		cheap[t0] = 0.10
	}
	if p := HeatTarget(c, s, l, HeatInput{Now: at(12, 0), Tin: 20, Tout: 5, Prices: prices(cheap)}); !p.PreheatStart.Before(at(13, 40)) {
		t.Fatalf("goedkoper vroeger: %+v", p)
	}
	// weekend: hele dag comfort
	sat := time.Date(2026, 11, 7, 9, 0, 0, 0, bru)
	if p := HeatTarget(c, s, l, HeatInput{Now: sat, Tin: 21, Tout: 5}); p.Target != 22 {
		t.Fatalf("weekend: %+v", p)
	}
	// vrijdagavond na 22 u: volgende venster is zaterdag 07:00
	fri := time.Date(2026, 11, 6, 23, 0, 0, 0, bru)
	if p := HeatTarget(c, s, l, HeatInput{Now: fri, Tin: 21, Tout: 5}); !p.NextWindow.Equal(time.Date(2026, 11, 7, 7, 0, 0, 0, bru)) {
		t.Fatalf("volgend venster: %+v", p)
	}
}

func TestLearnedLead(t *testing.T) {
	l := defaultLearned()
	// gemeten: 20 min/°C bij 15 °C buiten, 40 min/°C bij 5 °C buiten
	for i := 0; i < 3; i++ {
		l.Preheat = append(l.Preheat, PreheatSample{Tout: 15, Rise: 2, Minutes: 40}, PreheatSample{Tout: 5, Rise: 2, Minutes: 80})
	}
	if v := l.LeadMinutes(5, 2); v < 87 || v > 89 { // 80 · 1,1 marge
		t.Fatalf("5 °C: %v", v)
	}
	if v := l.LeadMinutes(0, 2); v < 109 || v > 111 { // 50 min/°C · 2 · 1,1
		t.Fatalf("0 °C: %v", v)
	}
}

func TestScheduleModes(t *testing.T) {
	c, s, l := defaultConfig(), defaultSchedule(22, 20, 40, 48), defaultLearned()
	// eigen periode met een andere temperatuur: maandag 10–12 u 19 °C, boiler 07–08 u 50 °C
	s.Heat[0] = append(s.Heat[0], Period{10 * 60, 12 * 60, 19})
	s.DHW[0] = append(s.DHW[0], Period{7 * 60, 8 * 60, 50})
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if p := HeatTarget(c, s, l, HeatInput{Now: at(10, 30), Tin: 21, Tout: 5}); p.Target != 19 {
		t.Fatalf("eigen periode: %+v", p)
	}
	if p := DHWTarget(c, s, l, DHWInput{Now: at(7, 0), Temp: 46}); p.Target < 48 {
		t.Fatalf("eigen boilerperiode (07–08 u ≥ 50 °C, nu 46): %v %s", p.Target, p.Why)
	}
	// vakantie: 16 °C, boiler gewoon, ook 's avonds en 's nachts
	s.Mode = ModeVacation
	if p := HeatTarget(c, s, l, HeatInput{Now: at(18, 0), Tin: 21, Tout: 5}); p.Target != 16 {
		t.Fatalf("vakantie: %+v", p)
	}
	if p := DHWTarget(c, s, l, DHWInput{Now: at(1, 30), Temp: 50}); p.Target != 40 || len(p.Heat) != 0 {
		t.Fatalf("vakantie: %v %s", p.Heat, p.Why)
	}
	// zomer: verwarming uit, boiler volgens het schema
	s.Mode = ModeSummer
	if p := HeatTarget(c, s, l, HeatInput{Now: at(18, 0), Tin: 21, Tout: 5}); p.Target != 0 {
		t.Fatalf("zomer: %+v", p)
	}
	p := DHWTarget(c, s, l, DHWInput{Now: at(16, 0), Temp: 44})
	assertFeasible(t, s, p, "zomer")
}

func TestScheduleJSON(t *testing.T) {
	s := defaultSchedule(22, 20, 40, 48)
	b, _ := json.Marshal(s)
	if !strings.Contains(string(b), `{"from":"06:00","to":"07:30","temp":22}`) {
		t.Fatalf("json: %s", b)
	}
	var r Schedule
	if err := json.Unmarshal(b, &r); err != nil || r.Heat[5][0] != (Period{7 * 60, 22 * 60, 22}) {
		t.Fatalf("terug: %v %+v", err, r.Heat[5])
	}
	bad := s
	bad.Heat[0] = []Period{{6 * 60, 9 * 60, 22}, {8 * 60, 10 * 60, 22}}
	if bad.Validate() == nil {
		t.Fatal("overlap niet gezien")
	}
	if json.Unmarshal([]byte(`{"from":"25:00","to":"26:00","temp":20}`), &Period{}) == nil {
		t.Fatal("ongeldig uur niet gezien")
	}
	full := s
	full.Heat[6] = []Period{{0, 1440, 21}}
	if err := full.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestChargePlan(t *testing.T) {
	if v := nextPlanTime(at(22, 0), "07:00"); !v.Equal(time.Date(2026, 11, 3, 7, 0, 0, 0, bru)) {
		t.Fatalf("avond: %v", v)
	}
	if v := nextPlanTime(at(1, 0), "07:00"); !v.Equal(at(7, 0)) {
		t.Fatalf("nacht: %v", v)
	}
	if v := nextPlanTime(at(6, 45), "07:00"); !v.Equal(time.Date(2026, 11, 3, 7, 0, 0, 0, bru)) {
		t.Fatalf("te kort dag: %v", v)
	}
	for _, c := range []struct {
		m, p string
		ok   bool
	}{
		{"DELETE", "/api/loadpoints/1/plan/energy", true},
		{"POST", "/api/batterygridchargelimit/0.2", true},
		{"GET", "/api/loadpoints/1/plan", false},
		{"POST", "/api/config/site", false},
		{"DELETE", "/api/loadpoints/../config", false},
	} {
		if evccProxyAllowed(c.m, c.p) != c.ok {
			t.Fatalf("proxy %s %s", c.m, c.p)
		}
	}
	p := ChargePlan{On: true, KWh: 16, Time: "07:00"}
	if p.Validate() != nil {
		t.Fatal("geldig plan geweigerd")
	}
	p.Time = "24:00"
	if p.Validate() == nil {
		t.Fatal("24:00 aanvaard")
	}
}

func TestLearnDHWUse(t *testing.T) {
	st = State{Learned: defaultLearned()}
	// dinsdag 20:00–20:15: de boiler zakt van 50 naar 44 °C zonder op te warmen (douche)
	tue := time.Date(2026, 11, 3, 20, 0, 0, 0, bru)
	learnDHWUse(tue.Add(time.Minute), 50, false)
	learnDHWUse(tue.Add(8*time.Minute), 47, false)
	learnDHWUse(tue.Add(15*time.Minute), 44, false)
	u := st.Learned.use()
	prior := defaultDHWUse()[1][80]
	want := 0.75*prior + 0.25*(6-st.Learned.lossPerQuarter(50))
	if d := u[1][80] - want; d > 1e-9 || d < -1e-9 {
		t.Fatalf("verbruik geleerd: %.3f, verwacht %.3f", u[1][80], want)
	}
	// een kwartier met opwarmen leert niets
	learnDHWUse(tue.Add(16*time.Minute), 44, true)
	learnDHWUse(tue.Add(30*time.Minute), 48, false)
	if st.Learned.use()[1][81] != prior {
		t.Fatalf("opwarmkwartier geleerd: %v", st.Learned.use()[1][81])
	}
}

func TestForecastRoom(t *testing.T) {
	c, s, l := defaultConfig(), defaultSchedule(22, 20, 40, 48), defaultLearned()
	room, set := forecastRoom(c, s, l, at(13, 0), 20, 5, 20, nil)
	idx := func(h, m int) int { return int(at(h, m).Sub(at(13, 0)) / quarter) }
	if set[idx(13, 0)] != 20 || set[idx(13, 45)] != 22 {
		t.Fatalf("ingesteld: %v", set[:12])
	}
	if room[idx(15, 45)] < 21.9 {
		t.Fatalf("om 16:00 nog maar %.1f °C", room[idx(15, 45)])
	}
	// na 22 u eco: langzaam afkoelen, nooit onder de ingestelde 20 °C
	if r := room[idx(23, 45)]; r > 22 || r < 21 {
		t.Fatalf("afkoelen: 00:00 %.1f °C", r)
	}
	for i, r := range room {
		if r < 20-1e-9 {
			t.Fatalf("onder eco op kwartier %d: %.1f", i, r)
		}
	}
}

func TestBattDecide(t *testing.T) {
	for _, c := range []struct {
		advice    string
		grid, bat float64
		car       float64
		mode      string
	}{
		{"charge", 800, 0, 0, "charge"},
		{"charge", 3000, 0, 0, "hold"},       // huis 3 kW + 3,5 kW laden: te veel
		{"charge", -2500, 3500, 0, "charge"}, // batterij ontlaadt nu 3,5 kW: het huis zelf neemt maar 1 kW
		{"charge", 4800, 0, 0, ""},           // kwartierpiek: de batterij mag helpen
		{"hold", 300, 0, 0, "hold"},
		{"hold", 2000, 3000, 3600, ""}, // huis + auto 5 kW: piek afvlakken
		{"holdcharge", -2000, 0, 0, "holdcharge"},
		{"discharge", -500, 0, 0, ""},
		{"normal", 0, 0, 0, ""},
		{"normal", 300, 3500, 3700, "hold"}, // auto laadt uit de batterij: vasthouden
		{"", 0, 0, 0, ""},
	} {
		if m, why := battDecide(c.advice, battHouse{Grid: c.grid, Batt: c.bat, Car: c.car}); m != c.mode {
			t.Fatalf("%s bij net %v, batterij %v, auto %v W: %q (%s)", c.advice, c.grid, c.bat, c.car, m, why)
		}
	}
}

func TestDST(t *testing.T) {
	// zondag 25 oktober 2026: om 03:00 wordt het 02:00 (wintertijd)
	sat := time.Date(2026, 10, 24, 22, 0, 0, 0, bru)
	if v := nextPlanTime(sat, "07:00"); v.Hour() != 7 || v.Day() != 25 {
		t.Fatalf("laadplan op de wintertijddag: %v", v)
	}
	sun := time.Date(2026, 10, 25, 12, 0, 0, 0, bru)
	if v := atMinute(sun, 16*60); v.Hour() != 16 {
		t.Fatalf("16:00 op de wintertijddag: %v", v)
	}
	if v := atMinute(sun, 1440); !v.Equal(time.Date(2026, 10, 26, 0, 0, 0, 0, bru)) {
		t.Fatalf("24:00: %v", v)
	}
	// maart: zomertijd
	if v := atMinute(time.Date(2026, 3, 29, 12, 0, 0, 0, bru), 7*60); v.Hour() != 7 {
		t.Fatalf("07:00 op de zomertijddag: %v", v)
	}
}

func TestCarFull(t *testing.T) {
	t0 := at(5, 0)
	lp := evccLoadpoint{Connected: true, Enabled: true, ChargedEnergy: 9800, ChargePower: 0}
	idle, full := carFull(time.Time{}, false, t0, lp)
	if full {
		t.Fatal("meteen vol")
	}
	if _, full = carFull(idle, full, t0.Add(11*time.Minute), lp); !full {
		t.Fatal("na 11 min niet vol")
	}
	// pauze voor de kwartierpiek: evcc laat niet laden → niet vol
	pause := evccLoadpoint{Connected: true, Enabled: false, ChargedEnergy: 9800}
	if _, f := carFull(time.Time{}, false, t0.Add(30*time.Minute), pause); f {
		t.Fatal("pauze telt als vol")
	}
	// laadpaal start niet (niets geladen) → niet vol
	if _, f := carFull(t0, false, t0.Add(30*time.Minute), evccLoadpoint{Connected: true, Enabled: true}); f {
		t.Fatal("niets geladen telt als vol")
	}
	// opnieuw laden of uitpluggen → weg
	if _, f := carFull(idle, true, t0, evccLoadpoint{Connected: true, Enabled: true, ChargePower: 3500}); f {
		t.Fatal("laadt weer, toch vol")
	}
	if _, f := carFull(idle, true, t0, evccLoadpoint{}); f {
		t.Fatal("uitgeplugd, toch vol")
	}
}

func TestLegioStart(t *testing.T) {
	now := at(3, 0)
	day := 24 * time.Hour
	pr := prices(map[time.Time]float64{at(4, 0): 0.10})
	if ok, _ := legioStart(now, now.Add(-6*day), pr, nil, 500, 50); ok {
		t.Fatal("na 6 dagen al een beurt")
	}
	// 7 dagen, nacht, geen zon, batterij half: wachten
	if ok, why := legioStart(now, now.Add(-7*day-time.Hour), pr, nil, 500, 50); ok {
		t.Fatal("7 dagen 's nachts zonder zon begonnen: " + why)
	}
	// 7 dagen, middag met zon
	sun := []Rate{{at(12, 0), at(13, 0), 4200}}
	if ok, why := legioStart(at(12, 15), now.Add(-7*day), pr, sun, 500, 50); !ok {
		t.Fatal("zon, toch niet begonnen: " + why)
	}
	// 7 dagen, overdag, volle batterij
	if ok, _ := legioStart(at(11, 0), now.Add(-7*day), pr, nil, 500, 90); !ok {
		t.Fatal("volle batterij overdag, toch niet begonnen")
	}
	// 8 dagen: goedkoopste kwartier (04:00), niet om 03:00
	old := now.Add(-8*day - time.Hour)
	if ok, _ := legioStart(now, old, pr, nil, 500, 20); ok {
		t.Fatal("8 dagen: begonnen voor het goedkoopste kwartier")
	}
	if ok, why := legioStart(at(4, 0), old, pr, nil, 500, 20); !ok {
		t.Fatal("8 dagen: goedkoopste kwartier niet gebruikt: " + why)
	}
	// 9 dagen: meteen, behalve bij een hoge afname
	if ok, _ := legioStart(now, now.Add(-9*day), pr, nil, 500, 20); !ok {
		t.Fatal("9 dagen: niet begonnen")
	}
	if ok, _ := legioStart(now, now.Add(-9*day), pr, nil, 4000, 20); ok {
		t.Fatal("begonnen terwijl het huis 4 kW afneemt")
	}
}

func TestRoomOffset(t *testing.T) {
	// warmtepomp toont hele graden (afgerond), Shelly 20,0 → 21,0 °C: gemiddeld verschil ±0
	off, n := 0.0, 0
	for i := 0; i < 600; i++ {
		sh := 20 + float64(i%100)/100
		off = roomOffset(off, n, math.Round(sh), sh)
		n++
	}
	if math.Abs(off) > 0.15 {
		t.Fatalf("verschil %.2f, verwacht ±0", off)
	}
	// warmtepompsensor zit 1 °C hoger
	for i := 0; i < 600; i++ {
		sh := 20 + float64(i%100)/100
		off = roomOffset(off, n, math.Round(sh+1), sh)
		n++
	}
	if math.Abs(off-1) > 0.15 {
		t.Fatalf("verschil %.2f, verwacht 1", off)
	}
}

func TestLegionellaRun(t *testing.T) {
	evccURL = "http://127.0.0.1:1" // geen evcc
	now := at(3, 0)
	st = State{LegioLast: now.Add(-9*24*time.Hour - time.Hour)}
	if on, _ := legionella(now, 45, 40, nil, nil); !on {
		t.Fatal("niet begonnen")
	}
	if on, _ := legionella(now.Add(time.Hour), 59, 60, nil, nil); on || !st.LegioLast.Equal(now.Add(time.Hour)) {
		t.Fatal("niet gestopt bij 59 °C")
	}
	// lukt niet binnen 3 uur: stoppen en pas 12 uur later opnieuw
	st = State{LegioLast: now.Add(-10 * 24 * time.Hour), LegioOn: true, LegioStart: now}
	if on, _ := legionella(now.Add(3*time.Hour+time.Minute), 55, 60, nil, nil); on {
		t.Fatal("na 3 uur niet gestopt")
	}
	if on, _ := legionella(now.Add(5*time.Hour), 50, 40, nil, nil); on {
		t.Fatal("te snel opnieuw")
	}
	if on, _ := legionella(now.Add(16*time.Hour), 50, 40, nil, nil); !on {
		t.Fatal("niet opnieuw geprobeerd")
	}
}

func TestShellyPush(t *testing.T) {
	sh = shellyData{}
	h := http.NewServeMux()
	serveShelly(h)
	for _, u := range []string{"/shelly?temp=${ev.tC}", "/shelly?hum=55.5", "/shelly?temp=20,7"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", u, nil))
	}
	m, ok := shellyNow(time.Now().Add(3 * time.Hour))
	if !ok || m.Temp != 20.7 || m.Hum != 55.5 {
		t.Fatalf("meting %+v ok=%v", m, ok)
	}
	if _, ok := shellyNow(time.Now().Add(5 * time.Hour)); ok {
		t.Fatal("meting van 5 uur oud nog gebruikt")
	}
}

func TestAggregateHistory(t *testing.T) {
	var rows []histRow
	for i := 0; i < 12; i++ { // 1 uur, elke 5 min
		r := histRow{T: at(10, 0).Add(time.Duration(i) * 5 * time.Minute), Room: 20 + float64(i%2), RoomSet: 20, DHW: 50}
		if i == 7 {
			r.RoomSet, r.RoomManual = 22, true
		}
		rows = append(rows, r)
	}
	a := aggregateHistory(rows, 30*time.Minute)
	if len(a) != 2 || a[0].Room != 20.5 || a[1].RoomSet != 22 || !a[1].RoomManual || a[0].RoomManual || a[0].Shelly != 0 {
		t.Fatalf("%+v", a)
	}
}

func TestBattSmooth(t *testing.T) {
	t0 := at(12, 0)
	if m := battSmooth("hold", t0, "charge", t0.Add(time.Minute), false); m != "hold" {
		t.Fatal("meteen weer laden na vasthouden")
	}
	if m := battSmooth("hold", t0, "charge", t0.Add(6*time.Minute), false); m != "charge" {
		t.Fatal("na 6 min nog niet laden")
	}
	if m := battSmooth("charge", t0, "hold", t0.Add(time.Minute), false); m != "hold" {
		t.Fatal("laden niet meteen gestopt")
	}
	if m := battSmooth("hold", t0, "", t0.Add(time.Minute), false); m != "hold" {
		t.Fatal("vasthouden meteen losgelaten")
	}
	if m := battSmooth("hold", t0, "", t0.Add(time.Minute), true); m != "" {
		t.Fatal("bij de kwartierpiek niet meteen gewone werking")
	}
}

func TestDishPlan(t *testing.T) {
	dur := 3 * time.Hour
	// 's avonds ingeladen, nacht 02:00–05:00 goedkoop: daar starten (klaar tegen 07:00)
	night := map[time.Time]float64{}
	for q := at(2, 0); q.Before(at(5, 0)); q = q.Add(quarter) {
		night[q] = 0.10
	}
	pr := prices(night)
	s, why := dishPlan(at(0, 30), dur, "07:00", pr, nil, nil)
	if !s.Equal(at(2, 0)) {
		t.Fatalf("start %s (%s), verwacht 02:00", s.Format("15:04"), why)
	}
	// te laat voor 07:00: meteen
	if s, _ := dishPlan(at(5, 0), dur, "07:00", pr, nil, nil); !s.Equal(at(5, 0)) {
		t.Fatalf("start %s, verwacht meteen", s.Format("15:04"))
	}
	// met zon 12:00–15:00 en een lage terugleverprijs: dan starten (klaar tegen 18:00)
	sun := []Rate{{at(12, 0), at(15, 0), 4000}}
	fi := []Rate{{at(0, 0), at(23, 59), 0.02}}
	if s, why := dishPlan(at(8, 0), dur, "18:00", prices(nil), fi, sun); !s.Equal(at(12, 0)) {
		t.Fatalf("start %s (%s), verwacht 12:00 (zon)", s.Format("15:04"), why)
	}
	// zonder prijzen: meteen
	if s, _ := dishPlan(at(8, 0), dur, "18:00", nil, nil, nil); !s.Equal(at(8, 0)) {
		t.Fatal("zonder prijzen niet meteen")
	}
}
