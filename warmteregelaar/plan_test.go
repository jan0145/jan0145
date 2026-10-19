package main

import (
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
	c, l := defaultConfig(), defaultLearned()
	if v, _ := DHWTarget(c, l, DHWInput{Now: at(11, 0), Temp: 45}); v != 40 {
		t.Fatalf("overdag: %v", v)
	}
	if v, _ := DHWTarget(c, l, DHWInput{Now: at(18, 0), Temp: 47}); v != 48 {
		t.Fatalf("avond: %v", v)
	}
	if v, w := DHWTarget(c, l, DHWInput{Now: at(11, 0), Temp: 45, PVBoost: true}); v != 55 || !strings.Contains(w, "zon") {
		t.Fatalf("zonne-overschot: %v %s", v, w)
	}
}

func TestDHWNight(t *testing.T) {
	c, l := defaultConfig(), defaultLearned()
	// goedkoopst: 03:00–04:15 (5 kwartieren; nodig: (55-48+2)/8*4 = 4,5 → 5)
	cheap := map[time.Time]float64{}
	for i := 0; i < 5; i++ {
		cheap[at(3, 0).Add(time.Duration(i)*15*time.Minute)] = 0.10
	}
	pr := prices(cheap)
	if v, _ := DHWTarget(c, l, DHWInput{Now: at(1, 0), Temp: 44, Prices: pr}); v != 40 {
		t.Fatalf("01:00 is duur: %v", v)
	}
	if v, w := DHWTarget(c, l, DHWInput{Now: at(3, 20), Temp: 44, Prices: pr}); v != 55 {
		t.Fatalf("03:20 is goedkoop: %v %s", v, w)
	}
	// begonnen beurt loopt door als het goedkope blok voorbij is
	if v, w := DHWTarget(c, l, DHWInput{Now: at(4, 20), Temp: 50, Prev: 55, Prices: pr}); v != 55 || !strings.Contains(w, "loopt door") {
		t.Fatalf("doorlopen: %v %s", v, w)
	}
	if v, _ := DHWTarget(c, l, DHWInput{Now: at(4, 20), Temp: 54.5, Prev: 55, Prices: pr}); v != 40 {
		t.Fatalf("warm genoeg: %v", v)
	}
	// veel zon verwacht morgen → geen nachtbeurt
	sun := []Rate{{at(10, 0), at(16, 0), 3000}} // 18 kWh
	if v, _ := DHWTarget(c, l, DHWInput{Now: at(3, 20), Temp: 44, Prices: pr, Solar: sun}); v != 40 {
		t.Fatalf("zon morgen: %v", v)
	}
	// geen prijzen → vaste nachtbeurt 01:00–02:30
	if v, _ := DHWTarget(c, l, DHWInput{Now: at(1, 30), Temp: 44}); v != 55 {
		t.Fatalf("vast uur: %v", v)
	}
}

func TestDHWBeforeEvening(t *testing.T) {
	c, l := defaultConfig(), defaultLearned()
	// 44 °C om 15:00, afkoeling 0,4/u → 43,2 om 17 u: bijwarmen tot 52, nodig 5 kwartieren
	cheap := map[time.Time]float64{at(15, 0): 0.05, at(15, 15): 0.05, at(15, 30): 0.05, at(15, 45): 0.05, at(16, 0): 0.05}
	pr := prices(cheap)
	if v, w := DHWTarget(c, l, DHWInput{Now: at(15, 5), Temp: 44, Prices: pr}); v != 52 {
		t.Fatalf("bijwarmen: %v %s", v, w)
	}
	// warm genoeg: niets doen
	if v, _ := DHWTarget(c, l, DHWInput{Now: at(15, 5), Temp: 51, Prices: pr}); v != 40 {
		t.Fatalf("warm genoeg: %v", v)
	}
	// te laat om te wachten: meteen
	if v, _ := DHWTarget(c, l, DHWInput{Now: at(16, 30), Temp: 42, Prices: prices(nil)}); v != 52 {
		t.Fatalf("laatste moment: %v", v)
	}
}

func TestHeat(t *testing.T) {
	c, l := defaultConfig(), defaultLearned()
	if p := HeatTarget(c, l, HeatInput{Now: at(6, 30), Tin: 21, Tout: 5}); p.Target != 22 {
		t.Fatalf("ochtendvenster: %+v", p)
	}
	if p := HeatTarget(c, l, HeatInput{Now: at(10, 0), Tin: 21, Tout: 5}); p.Target != 20 {
		t.Fatalf("overdag eco: %+v", p)
	}
	// 2 °C bijwarmen bij 5 °C buiten: (30+4·10)·2 = 140 min → start 13:40 voor het venster van 16:00
	p := HeatTarget(c, l, HeatInput{Now: at(13, 0), Tin: 20, Tout: 5})
	if p.Target != 20 || p.LeadMinutes != 140 || !p.PreheatStart.Equal(at(13, 40)) {
		t.Fatalf("nog geen voorverwarming: %+v", p)
	}
	if p := HeatTarget(c, l, HeatInput{Now: at(13, 45), Tin: 20, Tout: 5}); p.Target != 22 {
		t.Fatalf("voorverwarmen: %+v", p)
	}
	// goedkoper 30 min vroeger → vroeger starten
	cheap := map[time.Time]float64{}
	for t0 := at(13, 0); t0.Before(at(15, 30)); t0 = t0.Add(15 * time.Minute) {
		cheap[t0] = 0.10
	}
	if p := HeatTarget(c, l, HeatInput{Now: at(12, 0), Tin: 20, Tout: 5, Prices: prices(cheap)}); !p.PreheatStart.Before(at(13, 40)) {
		t.Fatalf("goedkoper vroeger: %+v", p)
	}
	// weekend: hele dag comfort
	sat := time.Date(2026, 11, 7, 9, 0, 0, 0, bru)
	if p := HeatTarget(c, l, HeatInput{Now: sat, Tin: 21, Tout: 5}); p.Target != 22 {
		t.Fatalf("weekend: %+v", p)
	}
	// vrijdagavond na 22 u: volgende venster is zaterdag 07:00
	fri := time.Date(2026, 11, 6, 23, 0, 0, 0, bru)
	if p := HeatTarget(c, l, HeatInput{Now: fri, Tin: 21, Tout: 5}); !p.NextWindow.Equal(time.Date(2026, 11, 7, 7, 0, 0, 0, bru)) {
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
