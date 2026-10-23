package main

import (
	"encoding/json"
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

func TestDHWWindows(t *testing.T) {
	c, s, l := defaultConfig(), defaultSchedule(22, 20, 40, 48), defaultLearned()
	if v, _ := DHWTarget(c, s, l, DHWInput{Now: at(11, 0), Temp: 45}); v != 40 {
		t.Fatalf("overdag: %v", v)
	}
	if v, _ := DHWTarget(c, s, l, DHWInput{Now: at(18, 0), Temp: 47}); v != 48 {
		t.Fatalf("avond: %v", v)
	}
	if v, w := DHWTarget(c, s, l, DHWInput{Now: at(11, 0), Temp: 45, PVBoost: true}); v != 55 || !strings.Contains(w, "zon") {
		t.Fatalf("zonne-overschot: %v %s", v, w)
	}
}

func TestDHWNight(t *testing.T) {
	c, s, l := defaultConfig(), defaultSchedule(22, 20, 40, 48), defaultLearned()
	// goedkoopst: 03:00–04:15 (5 kwartieren; nodig: (55-48+2)/8*4 = 4,5 → 5)
	cheap := map[time.Time]float64{}
	for i := 0; i < 5; i++ {
		cheap[at(3, 0).Add(time.Duration(i)*15*time.Minute)] = 0.10
	}
	pr := prices(cheap)
	if v, _ := DHWTarget(c, s, l, DHWInput{Now: at(1, 0), Temp: 44, Prices: pr}); v != 40 {
		t.Fatalf("01:00 is duur: %v", v)
	}
	if v, w := DHWTarget(c, s, l, DHWInput{Now: at(3, 20), Temp: 44, Prices: pr}); v != 55 {
		t.Fatalf("03:20 is goedkoop: %v %s", v, w)
	}
	// begonnen beurt loopt door als het goedkope blok voorbij is
	if v, w := DHWTarget(c, s, l, DHWInput{Now: at(4, 20), Temp: 50, Prev: 55, Prices: pr}); v != 55 || !strings.Contains(w, "loopt door") {
		t.Fatalf("doorlopen: %v %s", v, w)
	}
	if v, _ := DHWTarget(c, s, l, DHWInput{Now: at(4, 20), Temp: 54.5, Prev: 55, Prices: pr}); v != 40 {
		t.Fatalf("warm genoeg: %v", v)
	}
	// veel zon verwacht morgen → geen nachtbeurt
	sun := []Rate{{at(10, 0), at(16, 0), 3000}} // 18 kWh
	if v, _ := DHWTarget(c, s, l, DHWInput{Now: at(3, 20), Temp: 44, Prices: pr, Solar: sun}); v != 40 {
		t.Fatalf("zon morgen: %v", v)
	}
	// geen prijzen → vaste nachtbeurt 01:00–02:30
	if v, _ := DHWTarget(c, s, l, DHWInput{Now: at(1, 30), Temp: 44}); v != 55 {
		t.Fatalf("vast uur: %v", v)
	}
}

func TestDHWBeforeEvening(t *testing.T) {
	c, s, l := defaultConfig(), defaultSchedule(22, 20, 40, 48), defaultLearned()
	// 44 °C om 15:00, afkoeling 0,4/u → 43,2 om 17 u (< 48 − 2): bijwarmen tot 48, nodig 2 kwartieren
	cheap := map[time.Time]float64{at(15, 0): 0.05, at(15, 15): 0.05, at(15, 30): 0.05, at(15, 45): 0.05, at(16, 0): 0.05}
	pr := prices(cheap)
	if v, w := DHWTarget(c, s, l, DHWInput{Now: at(15, 5), Temp: 44, Prices: pr}); v != 48 {
		t.Fatalf("bijwarmen: %v %s", v, w)
	}
	// warm genoeg: niets doen (ook 46,5 °C volstaat: de warmtepomp warmt zelf pas bij onder 46 °C)
	if v, _ := DHWTarget(c, s, l, DHWInput{Now: at(15, 5), Temp: 51, Prices: pr}); v != 40 {
		t.Fatalf("warm genoeg: %v", v)
	}
	if v, _ := DHWTarget(c, s, l, DHWInput{Now: at(15, 5), Temp: 47, Prices: pr}); v != 40 {
		t.Fatalf("warm genoeg: %v", v)
	}
	// een grote geleerde afkoeling (bad, douche) mag niet tot een overdreven voorspelling leiden
	big := l
	big.DHWCoolRate = 5
	if v, w := DHWTarget(c, s, big, DHWInput{Now: at(15, 5), Temp: 49, Prices: pr}); v != 40 {
		t.Fatalf("afkoeling begrensd: %v %s", v, w)
	}
	// te laat om te wachten: meteen
	if v, _ := DHWTarget(c, s, l, DHWInput{Now: at(16, 30), Temp: 42, Prices: prices(nil)}); v != 48 {
		t.Fatalf("laatste moment: %v", v)
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
	if v, _ := DHWTarget(c, s, l, DHWInput{Now: at(7, 30), Temp: 52}); v != 50 {
		t.Fatalf("eigen boilerperiode: %v", v)
	}
	// vakantie: 16 °C, boiler gewoon, ook 's avonds en 's nachts
	s.Mode = ModeVacation
	if p := HeatTarget(c, s, l, HeatInput{Now: at(18, 0), Tin: 21, Tout: 5}); p.Target != 16 {
		t.Fatalf("vakantie: %+v", p)
	}
	if v, _ := DHWTarget(c, s, l, DHWInput{Now: at(1, 30), Temp: 44}); v != 40 {
		t.Fatalf("vakantie nachtbeurt: %v", v)
	}
	if v, _ := DHWTarget(c, s, l, DHWInput{Now: at(18, 0), Temp: 44}); v != 40 {
		t.Fatalf("vakantie avond: %v", v)
	}
	// zomer: verwarming uit, boiler volgens het schema
	s.Mode = ModeSummer
	if p := HeatTarget(c, s, l, HeatInput{Now: at(18, 0), Tin: 21, Tout: 5}); p.Target != 0 {
		t.Fatalf("zomer: %+v", p)
	}
	if v, _ := DHWTarget(c, s, l, DHWInput{Now: at(18, 0), Temp: 47}); v != 48 {
		t.Fatalf("zomer boiler: %v", v)
	}
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
