# Warmteregelaar

Stuurt de boiler- en kamertemperatuur van de Panasonic-warmtepomp via de HeishaMon, met een **weekschema**
(aan te passen in de Stroom-app, tab Verwarming) in plaats van vaste uren. De regelaar kiest zelf wanneer hij opwarmt: met de kwartierprijzen en de
zonneprognose van evcc, en met wat hij gaandeweg leert over de boiler en het huis.

## Wat hij doet

**Sanitair warm water**
- Gewoon **40 °C**; in de periodes van het weekschema de temperatuur van die periode
  (standaard elke dag **17–22 u minstens 48 °C**: koken, afwas, douche).
- 's Nachts (00–07 u) één beurt tot **55 °C** in de **goedkoopste kwartieren** — niet als er de volgende dag
  tussen 10 en 16 u veel zon verwacht wordt (≥ 12 kWh), want dan doet de zon het.
- Bij **zonne-overschot** (evcc beslist, na de auto): 55 °C.
- In de 5 uur vóór een periode: zakt de boiler tegen het begin meer dan 2 °C onder de temperatuur van de periode
  (dan zou de warmtepomp zelf bijwarmen), dan al bijwarmen tot die temperatuur (niet warmer) in de goedkoopste
  kwartieren ervoor. De geleerde afkoeling telt daarbij voor hoogstens 1 °C per uur (tappen is geen afkoeling).
- Een begonnen beurt loopt door tot de boiler warm is (geen aan/uit-gependel).

**Verwarming**
- In de periodes van het weekschema de temperatuur van die periode; daarbuiten eco (**20 °C**).
  Standaard: weekdagen **06:00–07:30** en **16:00–22:00** 22 °C, weekend **07:00–22:00** 22 °C.
- Hij begint vooraf op te warmen, zodat het huis bij het begin van het venster warm is. Hoe lang dat duurt,
  **leert** hij: elke keer meet hij hoeveel minuten het huis nodig had per °C, bij welke buitentemperatuur.
  Zolang er nog geen 4 metingen zijn, rekent hij met een voorzichtige schatting.
- Is het tot een uur vroeger beginnen goedkoper, dan doet hij dat (rekening houdend met het extra verbruik).
- Een begonnen voorverwarming loopt door tot het begin van de periode (geen aan/uit-gependel).
- Zet iemand zelf de thermostaat anders (bediening of Stroom-app), dan geldt dat tot het volgende omslagpunt.
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

**Leren** (bewaard in `warmteregelaar-state.json`): hoe snel de boiler opwarmt en afkoelt, en hoe lang het
huis nodig heeft om op te warmen. Elke 5 minuten komt er een regel in `warmte-JJJJ-MM.csv` (buiten, kamer,
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

Instellingen (in de taak vóór `./warmteregelaar`): `BOILER_BOOST=55`. `COMFORT=22 ECO=20 BOILER=40 BOILER_AVOND=48`
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
