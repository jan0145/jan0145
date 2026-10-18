# evcc op de Synology DS214 – overstap van EVA naar evcc

evcc leest je P1-meter (net), je GoodWe (batterij) en je zonnepanelen (via de IoTaWatt).
We nemen de taken van EVA stap voor stap over:

| Stap | Wat | Status |
|---|---|---|
| 1 | **Laadpaal (RAEDIAN) + kwartierpiek begrenzen** | in `evcc.yaml` |
| 2 | Dynamisch tarief (laden en batterij op goedkope uren) | leverancier nodig |
| 3 | Batterij (GoodWe) van EVA naar evcc | voorbereid onderaan `evcc.yaml`, nog niet actief |
| 4 | Warmtepomp (Panasonic via HeishaMon): sanitair water op zonne-overschot | laadpunt **Boiler** (48 → 60 °C) |
| 5 | SolarEdge rechtstreeks via Modbus TCP | wacht op installateur |

**Stap 1 – kwartierpiek:** de `circuit` "aansluiting" bewaakt het vermogen op de P1-meter.
Komt het huis + de auto boven `maxPower`, dan verlaagt evcc de laadstroom van de laadpaal
(en pauzeert het laden als zelfs 6 A te veel is). Kies `maxPower` gelijk aan de kwartierpiek
die je niet wilt overschrijden. De batterij wordt in stap 1 nog niet aangestuurd: EVA blijft die beheren.
In het evcc-scherm zet je bij **Laadpaal → ⚙︎** de min. stroom op 6 A en de max. op 16 A.

**Volgorde bij zonne-overschot:** 1. batterij, 2. auto, 3. boiler.
- De **batterij** krijgt voorrang via het evcc-scherm: batterij-icoon → *Batterij-instellingen* →
  schuif "voorrang voor de batterij tot" op bv. 100 %. Dat werkt ook nu EVA de batterij nog beheert,
  want evcc rekent het laadvermogen van de batterij dan niet mee als overschot.
- De **auto** (`priority: 2`) en de **boiler** (`priority: 1`) staan in de modus *Zon*. In *Snel* laadt
  de auto altijd, ook zonder overschot (maar binnen de kwartierpiek-grens).
- De **boiler** is een schakelaar: bij overschot zet evcc het boilerdoel via de HeishaMon op 60 °C,
  daarna terug op 48 °C.

Je DS214 (DSM 6.2, ARM 32-bit, geen Docker) draait evcc als los programma via de Task Scheduler.

## 1. evcc downloaden (op je Mac)

1. Ga naar <https://github.com/evcc-io/evcc/releases/latest>.
2. Download onder **Assets** het bestand dat eindigt op **`linux-armv6.tar.gz`**
   (bv. `evcc_0.xxx.x_linux-armv6.tar.gz`). Niet amd64 of arm64: je Synology is 32-bit ARM.
3. Dubbelklik om uit te pakken. Je krijgt een map met een bestand **`evcc`**.

## 2. Bestanden op de Synology zetten

1. Maak in Finder de map `/Volumes/homes/docker/evcc/`.
2. Zet daarin:
   - het bestand **`evcc`** (uit stap 1),
   - **`evcc.yaml`** (uit deze map).
3. Open `evcc.yaml` met **TextEdit** en vervang:
   - `192.168.0.GGG` (3×) door het IP-adres van de **GoodWe**,
   - `192.168.0.III` (1×) door het IP-adres van de **IoTaWatt**,
   - `PV1` / `PV2` alleen als je kanalen in de IoTaWatt anders heten.

   Tip: in TextEdit eerst **Format → Make Plain Text**, en zet onder
   **Edit → Substitutions** de **Smart Quotes** uit. Anders worden de aanhalingstekens
   "krullend" en begrijpt evcc het bestand niet.

## 3. Taak aanmaken (DSM in het Engels)

**Control Panel → Task Scheduler → Create → Triggered Task → User-defined script**

- Tab **General**: Task: `evcc` · User: **root** · Event: **Boot-up**
- Tab **Task Settings** → *Run command* → *User-defined script*:

  ```
  pkill -x evcc; sleep 2; cd /volume1/homes/docker/evcc && chmod +x evcc && HOME=/volume1/homes/docker/evcc ./evcc --config evcc.yaml --database /volume1/homes/docker/evcc/evcc.db > evcc.log 2>&1 &
  ```

- **OK** (bevestig met je wachtwoord).
- Selecteer de taak → **Run**.

## 4. Controleren

- Open **`http://192.168.0.159:7070`** → je ziet het evcc-scherm met net, zon, batterij en huis.
- Lukt het niet, open dan `evcc.log` in dezelfde map en stuur de inhoud door.
- Kijk de eerste dag in de **EVA-app** of de batterij zich normaal gedraagt. evcc leest via
  dezelfde Modbus-aansluiting als EVA mogelijk doet; als EVA haperingen toont, stop evcc dan
  (Task Scheduler → taak uitschakelen en de Synology herstarten) en laat het weten.

## Opnieuw starten na een wijziging in evcc.yaml

Selecteer de taak in de Task Scheduler en klik op **Run**: het script stopt eerst de draaiende
evcc en start hem dan opnieuw.

`HOME=…` en `--database …` zijn nodig omdat de Task Scheduler geen persoonlijke map meegeeft;
zonder die twee stopt evcc met de fout `database exec: "getent": executable file not found`.

## Piekbewaker: kwartierpiek beperken met de warmtepomp

Volgorde als het kwartier boven de grens dreigt te gaan:
1. **de auto**: evcc verlaagt of stopt het laden (circuit `aansluiting`, `maxPower`);
2. **de warmtepomp stiller**: laadt de auto niet (meer) en dreigt het kwartiergemiddelde toch boven de grens
   te gaan, dan zet `piekbewaker.sh` de warmtepomp via de HeishaMon in stille modus 3 (minder vermogen);
3. **de warmtepomp uit** (alleen met `STAGE2=1`): als stille modus niet volstaat, maximaal 20 minuten.

Daarna zet het script alles terug zoals het was: bij een nieuw kwartier, of zodra de prognose ruim onder de grens
blijft. Tussen twee ingrepen zit minstens 3 minuten (spaart de compressor); tijdens ontdooien grijpt het niet in.

Installeren:
1. Zet `piekbewaker.sh` in `/volume1/homes/docker/evcc/` (vul bovenaan de adressen van P1-meter en HeishaMon in).
2. Task Scheduler → Create → Triggered Task → User-defined script, User **root**, Event **Boot-up**:

   ```
   pkill -f piekbewaker.sh; sleep 2; cd /volume1/homes/docker/evcc && ACT=0 sh piekbewaker.sh > /dev/null 2>&1 &
   ```
3. Selecteer de taak → **Run**. In `piekbewaker.log` zie je wat het script doet.

`ACT=0` is de **proefperiode**: het script schrijft alleen in het logboek wat het zou doen, zonder iets aan te
sturen. Ziet dat er een paar dagen goed uit, zet dan `ACT=1` (en eventueel `STAGE2=1`) in de taak en klik **Run**.

