# evcc op de Synology DS214 – alleen uitlezen

evcc leest je GoodWe (netmeter + batterij) via Modbus TCP en je zonnepanelen via de
IoTaWatt. **De configuratie kan niets aansturen**: er staat geen enkele schrijfopdracht in,
dus EVA blijft de batterij beheren.

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
