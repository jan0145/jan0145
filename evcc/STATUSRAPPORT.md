# Statusrapport

Doel: de behoefte dekken (warm huis, genoeg warm water, auto geladen) en daarvoor zo weinig mogelijk betalen,
zonder de kwartierpiek te overschrijden. Vraag af en toe "statusrapport" en stuur de bestanden hieronder: daaruit
kijk ik na of het plan werkt en wat er beter kan.

## Wat er draait

| Wat | Waar | Doet | Stand | Logboek |
|---|---|---|---|---|
| **evcc** 0.316.2 | Synology, poort 7070 | laadpaal (Slim, laadplan, prijsgrens), boiler bij zonne-overschot, batterij (GoodWe), kwartierpiek-grens 5 kW, Bolt-tarief (formule, echt vanaf 1/11), zonneprognose, SolarEdge via Modbus, Solis via de IoTaWatt | stuurt | `evcc.log` |
| **warmteregelaar** | Synology, poort 7080 | weekschema verwarming (voorverwarmen, geleerde opwarmtijd), plan sanitair warm water (voorspelling, goedkoopste kwartieren), zonne-overschot 55/60 °C, laadplan elke nacht, advies van de evcc-optimizer voor de batterij, enige lezer van de HeishaMon | stuurt (`ACT=1`), batterij proef (`BATT=0`) | `warmteregelaar.log`, `warmte-JJJJ-MM.csv` (elke 5 min), `warmteregelaar-state.json` (wat hij geleerd heeft) |
| **piekbewaker** | Synology | warmtepomp stil/uit als het kwartier toch boven 5 kW dreigt | proef (`ACT=0`) | `piekbewaker.log` |
| **HeishaMon** | warmtepomp (CN-NMODE) | uitlezen en commando's | – | – |
| **IoTaWatt** | groepenkast | groepen, Solis (PV2), Stroom-app | – | – |
| **Stroom-app** | IoTaWatt (`stroom.htm`) | Elektriciteit, Verwarming (manueel, weekschema, grafieken), Laadpaal (bediening) | – | – |

## Wat je stuurt

Alles staat op de Synology in `/volume1/homes/docker/evcc/` (File Station → homes → docker → evcc):

1. `evcc.log`
2. `warmteregelaar.log`
3. `piekbewaker.log`
4. `warmte-JJJJ-MM.csv` van deze (en eventueel vorige) maand
5. `warmteregelaar-state.json`
6. eventueel: de maandpiek uit de HomeWizard-app, en later de Bolt-factuur

## Wat ik ermee nakijk

- **Comfort**: zat de kamer in de periodes op temperatuur, en het sanitair water op minstens de ingestelde
  temperatuur (40 °C erbuiten)? Kwam er ergens manueel ingrijpen aan te pas?
- **Voorspelling**: klopten de voorspelde kamer- en boilertemperatuur? Zo niet: wat leert de regelaar verkeerd
  (opwarmtijd, warmteverlies, verbruik per weekdag)?
- **Kosten**: wanneer verbruikten warmtepomp, boiler en auto, tegen welke prijs; hoeveel in de goedkoopste
  kwartieren of met zon; werd er stroom teruggeleverd terwijl iets ze nuttig kon gebruiken?
- **Kwartierpiek**: hoogste kwartier van de maand; wat zou de piekbewaker gedaan hebben (→ `ACT=1`?).
- **Batterij**: de adviezen van de optimizer naast de prijzen en de zon (→ `BATT=1`?).
- **Laadpaal**: plannen gehaald, laden in goedkope uren, fouten van de Raedian.
- **Storingen**: HeishaMon, GoodWe (Modbus-fouten), SolarEdge, IoTaWatt.

## Openstaand

- [ ] Optimizer aanzetten in evcc (herinnering 23/10 17:00), daarna batterij-proef bekijken → `BATT=1`
- [ ] GoodWe-brug checken: tweede programma op de GoodWe? (herinnering 24/10 09:00)
- [ ] Piekbewaker van proef naar `ACT=1`
- [ ] Raedian: OCPP-profiel / autorisatie (installateur)
- [ ] 3×230V: evcc rekent 6 A × 3 fasen als ±4,1 kW (te hoog); laadvermogen bij 16 A nakijken
- [ ] Injectie afregelen bij negatieve terugleverprijs (SolarEdge, regel 7)
- [ ] 1 november: Bolt dynamisch, echte prijzen
- [ ] Winter: elektrische bijverwarming en kwartierpiek opvolgen
- [ ] evcc 0.317.2 beschikbaar (bijwerken na nakijken wat er verandert)
