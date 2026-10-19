# Energiestrategie: wat doet wat

Uitwerking van de strategie (dynamisch tarief, weersverwachting, verwacht verbruik, kwartierpiek) met evcc
als enige sturing. Per regel: hoe het gebeurt en wat er nog nodig is.

## Wat er al staat

- Kwartierpiek: evcc-circuit `aansluiting` (maxPower) remt de auto; `piekbewaker.sh` zet daarna de warmtepomp
  stiller/uit (proefmodus).
- Laadpaal via Modbus TCP, boiler (SWW 48 → 60 °C) via HeishaMon, prioriteit auto (2) > boiler (1).
- Bolt-tarief per kwartier met de exacte formules (afname incl. nettarieven en btw, injectie), klaar voor 1 november.
- Geschiedenis van alles in evcc; Stroom-app toont kwartierpiek, warmtepomp (COP), laadpaal.

## De regels

| # | Regel | Hoe | Nodig / status |
|---|---|---|---|
| 1 | PV-overschot: huis → auto → SWW-boost → batterij | evcc-prioriteiten: auto 2, boiler 1; batterij-voorrang (prioritySoc) **laag** zetten zodat de batterij als laatste komt | ✅ kan nu (instelling in het evcc-scherm) |
| 2 | Piekbudget winter 5 kW / zomer 3,5 kW; binnen het budget huis/WP → auto → batterij | circuit `maxPower` (auto); optimizer gebruikt dezelfde grens als harde importgrens; piekbewaker voor de WP | ✅ winter; zomer: `maxPower` in april naar 3500 |
| 3 | Nachtladen in de goedkoopste kwartieren | auto: **laadplan** (bv. 16 kWh tegen 07:00, kiest zelf de goedkoopste kwartieren); batterij: optimizer of "laden uit het net onder prijs X" | Bolt-tarief (1 nov), batterij onder evcc (stap 3) |
| 3b | Batterij niet leeg laten lopen in de auto | evcc "ontladen blokkeren tijdens snel/gepland laden" | stap 3 (batterij onder evcc) |
| 4 | SWW-beurt 's nachts zonder booster | vast nachtuur in de warmtepomp zelf (timer) of laadplan op de boiler; booster: instelling warmtepomp | HeishaMon-commando's moeten werken (CZ-TAW1-vraag) |
| 5 | Winterreserve: min. 50 % SOC 07–18 u | optimizer houdt zelf reserve als hij een avondpiek verwacht; anders kleine aanvulling op de piekbewaker (batterij "vasthouden" onder 50 % tussen 07 en 18 u) | stap 3 |
| 6 | Ontladen in de duurste kwartieren | optimizer (rekent met prijs, prognose en verbruiksprofiel) | stap 3 + keuze optimizer |
| 7 | Injectie afregelen bij negatieve injectieprijs, ná auto/batterij/SWW | evcc-exportgrens + SolarEdge-afregeling (sjabloon `solaredge`); Solis enkel via contactor | Modbus TCP SolarEdge (installateur), contactor Solis |
| 8 | Dimmen/booster uit alleen tegen de piek | piekbewaker (stille modus → uit); SG-ready via optionele print of HeishaMon | zie vragen |

## De optimizer van evcc

evcc heeft een (experimentele) optimizer die per kwartier plant met de dynamische prijzen, de zonneprognose,
het verbruiksprofiel van de laatste 4 weken, het rendement van de batterij en de piekgrens. Dat dekt regels 3, 5
en 6 grotendeels zonder eigen code. Voorwaarden:
- experimentele functies + optimizer aanzetten in evcc;
- een **sponsor-token** van evcc (GitHub-sponsoring, enkele euro per maand);
- de berekening gebeurt op `optimizer.evcc.io`: evcc stuurt daarvoor prijzen, prognose en verbruiksprofiel door.

Zonder optimizer: dezelfde regels met vaste drempels (laadplan auto, batterij laden onder een prijs,
ontladen blokkeren) plus een eigen regelaar voor reserve en ontladen in de duurste kwartieren. Werkt, maar minder slim.

## Volgorde

1. Nu: batterij-voorrang laag (regel 1), piekbewaker in proefmodus laten lopen.
2. 1 november: Bolt-tarief aan; auto met laadplan.
3. HeishaMon actief (zonder CZ-TAW1) → boiler, kamertemperatuur en piekbewaker echt laten werken.
4. EVA uit, batterij onder evcc (stap 3) → regels 3b, 5, 6; keuze optimizer.
5. SolarEdge Modbus → injectie afregelen (regel 7).
