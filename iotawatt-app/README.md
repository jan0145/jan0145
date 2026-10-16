# Stroom – IoTaWatt-app voor iPhone

Een eenvoudige web-app (één bestand) die laat zien hoeveel stroom je huis nu verbruikt,
hoeveel de zonnepanelen opwekken en welke groep het meest verbruikt.

- **Bovenaan:** verbruik, zonne-opbrengst en overschot/tekort, live (elke 5 seconden).
- **Waar gaat het naartoe?** Elke groep met een balkje, gesorteerd van groot naar klein.
  Kies **Nu** (watt) of **Vandaag** (kWh sinds middernacht).
- **Grafiek:** verbruik en zon over de dag. Tik of sleep erover om de waarden te zien.

## Installeren (5 minuten)

De app staat het best **op de IoTaWatt zelf**. Dan zijn er geen extra servers nodig en
werkt hij altijd zodra je thuis op wifi bent.

1. Open op je computer de IoTaWatt-webpagina (`http://iotawatt.local`, of het IP-adres).
2. Ga naar **Tools → File manager**.
3. Upload `stroom.htm` en `stroom.png` naar de hoofdmap (root) van de SD-kaart.
4. Open op je iPhone in **Safari**: `http://iotawatt.local/stroom.htm`
   (werkt `iotawatt.local` niet, gebruik dan het IP-adres, bv. `http://192.168.1.50/stroom.htm`).
5. Tik op **Deel** → **Zet op beginscherm**. Je hebt nu een app-icoon "Stroom".

## Eerste keer instellen

Tik op **⚙︎**. Alle kanalen die op je IoTaWatt een naam hebben verschijnen in de lijst.
Per kanaal kies je:

| Instelling | Betekenis |
|---|---|
| Naam | Hoe het kanaal in de app heet (leeg = de naam uit de IoTaWatt). |
| **Verbruik** | Een groep in de zekeringkast (verlichting, wasmachine, stopcontacten, …). |
| **Zon** | De lijn van de zonnepanelen. Beide zonnepaneel-kanalen worden opgeteld. Het teken (+/–) maakt niet uit. |
| **Hoofdaansluiting** | Alleen als je ook de netkabel meet. Dan toont de app afname/teruglevering en een regel "Overig (niet gemeten)". |
| **Verbergen** | Niet tonen en niet meetellen. |
| **×3 / ×1,73** | Voor de warmtepomp als je maar één draad meet. ×3 bij een 3N400V-net (4 draden met nul), ×1,73 (√3) bij een 3×230V-net (3 draden zonder nul). Dat klopt goed als de warmtepomp de fasen gelijk belast. |

De app doet een eerste gok op basis van de namen (bv. "Zon…" → Zon, "Warmtepomp…" → Verbruik ×3, "…total" of "rest" → Verbergen).
Een groep met een negatieve waarde krijgt een ⚠: dat wijst op een omgekeerde stroomtang of een verkeerde fase-instelling.
Instellingen worden op de iPhone bewaard.

**Let op:** zonder meting op de hoofdaansluiting is "Verbruik" de som van de groepen die je meet.
Het vakje rechtsboven (overschot/tekort) is dan een schatting.

## P1-meter en kwartierpiek

Vul bij ⚙︎ het adres van je **HomeWizard P1-meter** in (bv. `http://192.168.0.124`, met "Lokale API" aan).
De app gebruikt dan de P1-meter als officiële netmeting en toont een tegel **Kwartierpiek**:
het gemiddelde van het lopende kwartier, je hoogste kwartier van deze maand, en een waarschuwing
(oranje vanaf 85 %, rood boven je maandpiek; minimum 2,5 kW zoals het capaciteitstarief).

## Geschiedenis

- **Per groep en oudere dagen:** komt uit de IoTaWatt, die alle metingen jarenlang op zijn SD-kaart bewaart.
- **Net, batterij, echt huisverbruik en de laadpaal:** komen uit **evcc** op de Synology. evcc bewaart
  per kwartier hoeveel energie er van/naar het net en de batterij ging, wat het huis verbruikte en wat
  de laadpaal leverde. Die geschiedenis begint op de dag dat evcc (en de laadpaal erin) draait.
- In de grafiek betekent **negatief**: naar het net (teruglevering) of naar de batterij (laden).
- **Laadsessies:** sinds evcc de laadpaal aanstuurt, bewaart evcc elke laadbeurt (kWh, duur, % zon en,
  met een dynamisch tarief, de prijs). Oudere beurten zoekt de app in de kwartierwaarden van de laadpaal.

## Uitproberen zonder IoTaWatt

Open `stroom.htm?demo` in een browser, of vul `demo` in als adres bij ⚙︎.

## Goed om te weten

- Werkt alleen als je iPhone de IoTaWatt kan bereiken, dus thuis op wifi
  (of onderweg via een VPN naar huis).
- De app gebruikt de standaard IoTaWatt query-API (`/query?show=series` en `/query?select=…`);
  er wordt niets naar het internet gestuurd.
- Staat de pagina niet op de IoTaWatt zelf, dan kun je bij ⚙︎ het adres invullen. Je browser kan
  dat wel blokkeren (CORS / https → http), daarom is uploaden naar de IoTaWatt de aanbevolen manier.
