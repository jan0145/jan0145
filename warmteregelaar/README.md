# Warmteregelaar

Stuurt de boiler- en kamertemperatuur van de Panasonic-warmtepomp via de HeishaMon, met een **weekschema**
(aan te passen in de Stroom-app, tab Verwarming) in plaats van vaste uren. De regelaar kiest zelf wanneer hij opwarmt: met de kwartierprijzen en de
zonneprognose van evcc, en met wat hij gaandeweg leert over de boiler en het huis.

## Wat hij doet

**Sanitair warm water** (zie `sww.go`)
- De regelaar **voorspelt de boilertemperatuur** voor de komende 24 uur per kwartier: warmteverlies van het vat
  (hoe warmer, hoe meer), het **verbruik** (douches, bad, afwas) zoals hij het per weekdag en kwartier geleerd
  heeft, en hoe snel de warmtepomp de boiler opwarmt.
- Eis: in elke periode van het weekschema minstens de temperatuur van die periode (min 2 °C: bij 48 °C is 46 °C
  nog goed, zo werkt de warmtepomp ook), daarbuiten altijd minstens 40 °C.
- Waar de voorspelling te laag komt, kiest hij het goedkoopste kwartier ervóór om op te warmen: netprijs, of bij
  verwachte zon de gemiste terugleververgoeding. Warmer opwarmen telt duurder (lagere COP), een aparte beurt ook.
  Of het dus 's nachts tot 55 °C gaat en daarmee de hele dag, of 48 °C en later nog eens bij: dat rekent hij uit.
- Het plan en de reden (met de voorspelde temperatuur) staan in het logboek ("plan sww: …").
- Bij **zonne-overschot** (evcc beslist, na de auto): 55 °C. Bij veel overschot (≥ 3 kW teruglevering) of een
  negatieve terugleverprijs: 60 °C, dan helpt de elektrische bijverwarming van de boiler (goedkoper dan gratis of
  met verlies terugleveren), maar pas als de thuisbatterij ≥ 95 % is (voorrang: batterij → auto → boiler).
- Een begonnen beurt loopt door tot de boiler warm is (geen aan/uit-gependel).
- Vóór er iets geleerd is, rekent hij met een voorzichtig profiel: 's ochtends 2 °C, 's avonds 5 °C (weekend 8 °C).

**Verwarming**
- In de periodes van het weekschema de temperatuur van die periode; daarbuiten eco (**20 °C**).
  Standaard: weekdagen **06:00–07:30** en **16:00–22:00** 22 °C, weekend **07:00–22:00** 22 °C.
- Hij begint vooraf op te warmen, zodat het huis bij het begin van het venster warm is. Hoe lang dat duurt,
  **leert** hij: elke keer meet hij hoeveel minuten het huis nodig had per °C, bij welke buitentemperatuur.
  Zolang er nog geen 4 metingen zijn, rekent hij met een voorzichtige schatting.
- Is het tot een uur vroeger beginnen goedkoper, dan doet hij dat (rekening houdend met het extra verbruik).
- Een begonnen voorverwarming loopt door tot het begin van de periode (geen aan/uit-gependel).
- Zet iemand zelf de thermostaat anders (bediening of Stroom-app), dan geldt dat tot de volgende verandering in
  het programma: in een periode (of tijdens het voorverwarmen ervoor) tot het einde van die periode, daarbuiten tot
  het programma weer iets anders wil. Een nieuwe waarde op de bediening wordt niet meteen overschreven: de
  regelaar kijkt eerst of ze blijft staan.
- Staat de warmtepomp op een stand zonder sanitair water (bv. alleen verwarmen), dan zet de regelaar er sanitair
  water bij (behalve in de stand zomer); anders wordt de boiler nooit opgewarmd.
- Zet iemand zelf de boiler anders (bv. hoger voor een bad), dan volgt de regelaar dat tot de boiler warm is,
  hoogstens 3 uur. Na een eigen commando wacht de regelaar 2 minuten voor hij een afwijking als "zelf ingesteld"
  beschouwt (de HeishaMon toont een nieuwe waarde pas na 10–60 s).

**Stand** (knoppen in de app)
- **Normaal**: het weekschema.
- **Vakantie**: het schema staat uit: kamer op de vakantietemperatuur (standaard 16 °C), boiler gewoon (40 °C),
  geen nachtbeurt en geen bijwarmen. Zonne-overschot mag de boiler nog opwarmen (gratis).
- **Zomer**: de verwarming gaat uit: de regelaar zet de warmtepomp op "alleen sanitair water"
  (`SetOperationMode=3`). De boiler volgt het schema. Terug naar normaal: de warmtepomp gaat terug naar de stand
  van daarvoor (meestal verwarmen + sanitair water).
- Bij een andere stand vervallen de manuele aanpassingen.

**Leren** (bewaard in `warmteregelaar-state.json`): hoe snel de boiler opwarmt, het warmteverlies van het vat,
het warmwaterverbruik per weekdag en kwartier, en hoe lang het huis nodig heeft om op te warmen. Elke 5 minuten komt er een regel in `warmte-JJJJ-MM.csv` (buiten, kamer,
boiler, doelen, vermogen, prijs) — daarmee kan het model later verfijnd worden.

## Installeren op de Synology

1. Zet `warmteregelaar` in `/volume1/homes/docker/evcc/`.
2. Task Scheduler → Create → Triggered Task → User-defined script, User **root**, Event **Boot-up**:
   ```
   pkill -x warmteregelaar; sleep 2; cd /volume1/homes/docker/evcc && chmod +x warmteregelaar && ACT=0 HM=http://192.168.0.254 ./warmteregelaar >> warmteregelaar.log 2>&1 &
   ```
3. **Run**. In `warmteregelaar.log` zie je wat hij beslist; in de Stroom-app (Warmtepomp) staat een kaart
   "Warmteregelaar".

`ACT=0` = **proef**: hij beslist en logt, maar stuurt niets. Ziet het er goed uit: `ACT=1` en **Run**.
Let op: in de proef warmt de boiler ook niet op bij zonne-overschot (dat loopt nu via de regelaar).

Instellingen (in de taak vóór `./warmteregelaar`): `BOILER_BOOST=55` (zonne-overschot), `BOILER_BOOST_HOT=60`. `COMFORT=22 ECO=20 BOILER=40 BOILER_AVOND=48`
bepalen alleen het standaardschema; zodra het schema in de app bewaard is, geldt dat (in `warmteregelaar-state.json`).

Weekschema via HTTP: `GET /schedule` (lezen), `POST /schedule` (JSON, vervangen), `GET /mode?set=normaal|vakantie|zomer`.

## Eén lezer voor de HeishaMon

De HeishaMon is een klein wifi-bordje en verdraagt weinig gelijktijdige vragen. Daarom leest alleen de
regelaar hem uit (elke 10 s, `HMPOLL=10`) en geeft hij een kopie door:

- `http://<synology>:7080/json` — dezelfde gegevens als de HeishaMon (voor evcc en de Stroom-app);
- `http://<synology>:7080/command?…` — commando's worden doorgegeven (bv. kamertemperatuur vanuit de app).

In de Stroom-app vul je bij ⚙︎ → Warmtepomp dus `http://192.168.0.159:7080` in.

## Voor evcc

Het laadpunt "Boiler" in `evcc.yaml` geeft het zonne-overschot door aan de regelaar
(`http://127.0.0.1:7080/boiler`). De regelaar is de enige die de boiler aanstuurt.

## Bouwen

```
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 go build -trimpath -ldflags="-s -w" -o warmteregelaar .
```

## Laadplan voor de auto

evcc kent zonder voertuig alleen een eenmalig plan ("x kWh tegen hh:mm"). Staat in de Stroom-app (Laadpaal) "elke
nacht automatisch" aan, dan zet de regelaar dat plan elke keer opnieuw zodra de auto ingeplugd is
(`GET/POST /laadplan`). Wist je het plan zelf, dan zet hij het voor die ochtend niet opnieuw.
"Auto vol" toont de app alleen als het zeker is: evcc laat laden, er werd in deze laadbeurt al geladen (≥ 0,5 kWh)
en de auto neemt al 10 minuten geen stroom meer. Een pauze voor de kwartierpiek telt dus niet. (De laadpaal kent
de laadtoestand van de auto niet; daarvoor is de Volvo-koppeling van evcc nodig.)
Opdrachten die een browser niet rechtstreeks naar evcc mag sturen (een plan of prijsgrens wissen), geeft hij door
via `/evcc?m=DELETE&p=/api/…` (alleen laadpunten en prijsgrenzen).

## Grafieken

`GET /grafiek` geeft de laatste 24 uur (kamer, ingestelde kamertemperatuur, boiler, ingestelde boilertemperatuur,
uit de maandbestanden), de voorspelling voor de komende 24 uur en de voorspelling die 24 uur geleden gemaakt werd
(elk uur bewaard). De Stroom-app toont dat bij de warmtepomp: gemeten in kleur, voorspeld in grijs.
De kamervoorspelling: ingestelde temperatuur volgens het schema (met voorverwarmen), opwarmen met de geleerde
snelheid, afkoelen naar buiten toe met een geleerde afkoeling (uit de eco-uren).

## Thuisbatterij volgens de optimizer van evcc

De optimizer van evcc (experimenteel, met sponsor-token) rekent met prijzen, zonneprognose, verwacht verbruik en de
kwartierpiek uit wanneer de batterij best uit het net laadt, de lading vasthoudt of gewoon werkt. In evcc 0.316 is
dat nog alleen een advies. De regelaar volgt het (`batterij.go`):

- `BATT=0` (standaard): proef, in het logboek staat wat hij zou doen ("batterij (proef): …");
- `BATT=1`: hij zet de batterijmodus van evcc (`/api/batterymode/…`) en herhaalt dat elke 30 s. evcc zet een
  extern gezette modus na 1 minuut zelf terug naar normaal: valt de regelaar weg, dan werkt de batterij gewoon.
- Nooit uit het net laden als de afname al boven 4,5 kW zit (kwartierpiek); ontladen naar het net doet hij niet.

Voorwaarde: in evcc menu → Instellingen → "Experimentele functies" aan, daarna menu → Configuratie → "Optimizer" aan.
