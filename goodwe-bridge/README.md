# GoodWe-brug voor de Stroom-app

Een klein programma dat je GoodWe-batterijomvormer (bv. GW8K-BT) lokaal uitleest en de
gegevens doorgeeft aan de Stroom-app. Het draait op je Synology, die altijd aan staat.

Waarom een brug? De GoodWe praat via UDP (poort 8899). Een webpagina op je iPhone kan
dat niet. De brug vertaalt het naar een gewone webadres-vraag.
Alles blijft thuis: er gaat niets via de GoodWe-cloud en er zijn geen wachtwoorden nodig.

## Wat de app ermee doet

- **Netverbruik over alle drie de fasen** (van de GoodWe-meter), afname of teruglevering.
- **Batterij:** laadtoestand (%) en of hij laadt of ontlaadt.
- **Echt huisverbruik** = net + zonnepanelen + batterij (ontladen).
- **Overig (niet gemeten)** = huisverbruik − alle groepen die de IoTaWatt meet. Dit vervangt
  de "rest"-berekening op draad A.

## Installeren op de Synology zonder Container Manager (Taakplanner)

Werkt op elke Synology met DSM 7, ook modellen zonder Container Manager. De GoodWe-bibliotheek
zit mee in de map `goodwe/`. Je hebt wel Python 3.8 of nieuwer nodig: staat er in `bridge.log`
"Geen Python … gevonden" of "failed to run python3", installeer dan **Python 3** (bv. "Python 3.9")
via het **Package Center**. `start.sh` vindt die vanzelf.

1. **IP-adres van de GoodWe opzoeken** in je router of in de SolarGo-app (bij voorkeur een vast adres geven).
2. Kopieer de hele map `goodwe-bridge` (met `bridge.py`, `start.sh` en de map `goodwe`) naar de
   Synology, bv. naar de gedeelde map **homes** → `homes/docker/goodwe-bridge`.
   Op de Synology zelf heet dat pad dan `/volume1/homes/docker/goodwe-bridge`.
3. DSM → **Configuratiescherm → Taakplanner → Maken → Getriggerde taak → Door gebruiker gedefinieerd script**.
   - Tabblad *Algemeen*: taaknaam `GoodWe-brug`, gebruiker **root**, gebeurtenis **Opstarten**.
   - Tabblad *Taakinstellingen*, bij *Door gebruiker gedefinieerd script*:
     ```
     GOODWE_HOST=192.168.0.xxx sh /volume1/homes/docker/goodwe-bridge/start.sh
     ```
     (vervang `192.168.0.xxx` door het IP-adres van de **GoodWe**).
   - OK. DSM vraagt je wachtwoord ter bevestiging.
4. Selecteer de taak en klik op **Uitvoeren** (zodat je niet hoeft te herstarten).
5. **Testen:** `http://<IP-van-je-synology>:8765/goodwe.json` → `"ok": true`.
   Lukt het niet, kijk dan in `bridge.log` in dezelfde map.
6. Stroom-app → **⚙︎** → bij **GoodWe-batterij**: `http://<IP-van-je-synology>:8765` → Bewaar.

## Installeren op de Synology (Container Manager)

1. **IP-adres van de GoodWe opzoeken.** Kijk in je router (lijst met verbonden toestellen)
   of in de SolarGo-app. Geef de GoodWe bij voorkeur een vast IP-adres in je router.
2. Installeer **Container Manager** via het Package Center (als dat nog niet zo is).
3. Maak met **File Station** de map `docker/goodwe-bridge` aan en zet er
   `bridge.py` en `docker-compose.yml` in.
4. Open `docker-compose.yml` (rechtermuisklik → openen met Text Editor, of pas het vooraf
   aan op je computer) en vul bij `GOODWE_HOST` het IP-adres van de GoodWe in.
5. Container Manager → **Project** → **Maken**:
   - Projectnaam: `goodwe-bridge`
   - Pad: `docker/goodwe-bridge`
   - Kies "bestaand docker-compose.yml gebruiken" → Volgende → Klaar.
6. **Testen:** open in een browser `http://<IP-van-je-synology>:8765/goodwe.json`.
   Je ziet dan iets als `{"ok": true, "battery_w": -531, "battery_soc": 13, ...}`.
   Staat er `"ok": false`, kijk dan bij de foutmelding en in het logboek van de container.
7. Open de Stroom-app → **⚙︎** → vul bij **GoodWe-batterij** in:
   `http://<IP-van-je-synology>:8765` → Bewaar.

Gebruik je de firewall van de Synology, laat dan poort **8765** toe voor je thuisnetwerk.

## Controleren

- `http://<synology>:8765/raw` toont alle ruwe waarden van de GoodWe.
- Tekens in `goodwe.json`: `battery_w` positief = ontladen, negatief = laden;
  `grid_w` positief = afname van het net, negatief = teruglevering.
  Vergelijk één keer met de EVA- of SolarGo-app. Klopt een teken niet, laat het weten.

## Problemen

- **"timeout" / geen verbinding:** klopt het IP-adres? Sommige nieuwere GoodWe-dongles
  antwoorden alleen via Modbus/TCP: zet dan `GOODWE_PORT: "502"` in `docker-compose.yml`
  en herstart het project.
- **Geen Container Manager op je Synology-model?** Laat het weten, dan zoeken we een andere manier.
