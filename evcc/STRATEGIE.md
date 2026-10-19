# Energiestrategie: wat doet wat

Uitwerking van de strategie (dynamisch tarief, weersverwachting, verwacht verbruik, kwartierpiek) met evcc
als enige sturing. Per regel: hoe het gebeurt en wat er nog nodig is.

## Cijfers uit de factuur (augustus 2026, Fluvius Imewo)

- Capaciteitstarief: 437,08 € per jaar bij een gemiddelde maandpiek van **8,06 kW** → ≈ 54,2 €/kW/jaar excl. btw,
  ≈ **65,6 €/kW/jaar incl. 21 % btw** (≈ 5,5 €/kW/maand; de strategie rekende met 4,4).
  Het tarief gebruikt het gemiddelde van de maandpieken van de laatste 12 maanden: elke kW lagere maandpiek
  spaart ≈ 66 €/jaar zodra hij een jaar lang meetelt.
- Per kWh afname bovenop de energieprijs (excl. btw): afnametarief 0,0523 + groene stroom 0,0112 + WKK 0,0035
  + accijns 0,0142 + bijdrage 0,0019 = **0,0831 €/kWh**. Btw op afname: **21 %**.

## Wat er al staat

- Kwartierpiek: evcc-circuit `aansluiting` (maxPower) remt de auto; `piekbewaker.sh` zet daarna de warmtepomp
  stiller/uit (proefmodus).
- Laadpaal via Modbus TCP; boiler via de warmteregelaar (40 / 48 / 55 °C), prioriteit bij overschot auto (2) > boiler (1).
- Bolt-tarief per kwartier met de exacte formules (afname incl. nettarieven en 21 % btw, injectie), staat al aan
  zodat evcc nu al plant (tot 1 november zijn de bedragen een simulatie).
- Zonneprognose (open-meteo) voor Solis (zuid, 3,2 kWp) en SolarEdge (west, 5,5 kWp).
- Geschiedenis van alles in evcc; Stroom-app toont kwartierpiek, warmtepomp (COP), laadpaal.
- Warmteregelaar (`warmteregelaar/`): boiler en verwarming in tijdvensters, met prijzen, zonneprognose en zelf geleerde opwarmtijden.

## De regels

| # | Regel | Hoe | Nodig / status |
|---|---|---|---|
| 1 | PV-overschot: huis → auto → SWW-boost → batterij | evcc-prioriteiten: auto 2, boiler 1; batterij-voorrang (prioritySoc) **laag** zetten zodat de batterij als laatste komt | ✅ kan nu (instelling in het evcc-scherm) |
| 2 | Piekbudget winter 5 kW / zomer 3,5 kW; binnen het budget huis/WP → auto → batterij | circuit `maxPower` (auto); optimizer gebruikt dezelfde grens als harde importgrens; piekbewaker voor de WP | ✅ winter; zomer: `maxPower` in april naar 3500 |
| 3 | Nachtladen in de goedkoopste kwartieren | auto: **laadplan** (bv. 16 kWh tegen 07:00, kiest zelf de goedkoopste kwartieren); batterij: optimizer of "laden uit het net onder prijs X" | Bolt-tarief (1 nov), batterij onder evcc (stap 3) |
| 3b | Batterij niet leeg laten lopen in de auto | evcc "ontladen blokkeren tijdens snel/gepland laden" | stap 3 (batterij onder evcc) |
| 4 | SWW in vensters: gewoon 40 °C, 17–22 u ≥ 48 °C, nachtbeurt 55 °C in de goedkoopste kwartieren (niet bij veel zon morgen), zonne-overschot 55 °C | **warmteregelaar** (leert opwarm- en afkoelsnelheid van de boiler); evcc geeft het zonne-overschot door | ✅ gebouwd, proefmodus |
| 4b | Verwarming in vensters: weekdag 06–07:30 en 16–22 u 22 °C, weekend 07–22 u, anders 20 °C | **warmteregelaar** leert hoe lang het huis nodig heeft om op te warmen (per buitentemperatuur) en start op tijd, goedkoper vroeger als dat loont | ✅ gebouwd, proefmodus |
| 5 | Winterreserve: min. 50 % SOC 07–18 u | optimizer houdt zelf reserve als hij een avondpiek verwacht; anders kleine aanvulling op de piekbewaker (batterij "vasthouden" onder 50 % tussen 07 en 18 u) | stap 3 |
| 6 | Ontladen in de duurste kwartieren | optimizer (rekent met prijs, prognose en verbruiksprofiel) | stap 3 + keuze optimizer |
| 7 | Injectie afregelen bij negatieve injectieprijs, ná auto/batterij/SWW | evcc-exportgrens + SolarEdge-afregeling (sjabloon `solaredge`); Solis enkel via contactor | Modbus TCP SolarEdge (installateur), contactor Solis |
| 8 | Dimmen/booster uit alleen tegen de piek | piekbewaker (stille modus → uit); SG-ready via optionele print of HeishaMon | geen CZ-NS4P aanwezig (HeishaMon: Optional_PCB uit); HeishaMon kan de optionele print nabootsen |

## De optimizer van evcc

evcc heeft een (experimentele) optimizer die per kwartier plant met de dynamische prijzen, de zonneprognose,
het verbruiksprofiel van de laatste 4 weken, het rendement van de batterij en de piekgrens. Dat dekt regels 3, 5
en 6 grotendeels zonder eigen code. Voorwaarden:
- experimentele functies + optimizer aanzetten in evcc;
- een **sponsor-token** van evcc: het kleinste maandplan (4 $/maand via GitHub Sponsors) volstaat, het token
  staat daarna op sponsor.evcc.io (eenmalig ±150 € geeft een levenslang token);
- de berekening gebeurt op `optimizer.evcc.io`: evcc stuurt daarvoor prijzen, prognose en verbruiksprofiel door.

Zonder optimizer: dezelfde regels met vaste drempels (laadplan auto, batterij laden onder een prijs,
ontladen blokkeren) plus een eigen regelaar voor reserve en ontladen in de duurste kwartieren. Werkt, maar minder slim.

## Volgorde

1. Nu: batterij-voorrang laag (regel 1), piekbewaker in proefmodus laten lopen.
2. 1 november: Bolt-tarief aan; auto met laadplan.
3. ✅ HeishaMon actief (CZ-TAW1 eruit, commando's werken) → boiler, kamertemperatuur en piekbewaker.
4. EVA uit, batterij onder evcc (stap 3, `evcc-batterij.yaml` klaar en getest: vasthouden, laden uit het net) → regels 3b, 5, 6; keuze optimizer.
5. SolarEdge Modbus → injectie afregelen (regel 7).
