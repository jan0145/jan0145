#!/bin/sh
# Piekbewaker: houdt de kwartierpiek onder de grens.
#   1. De auto: dat doet evcc zelf (circuit "aansluiting" in evcc.yaml).
#   2. Is de auto al gestopt en dreigt het kwartier toch boven de grens te gaan,
#      dan zet dit script de warmtepomp via de HeishaMon in stille modus (minder vermogen).
#   3. Is dat nog niet genoeg (en STAGE2=1), dan gaat de warmtepomp even uit.
# Zodra het weer kan, zet het script alles terug zoals het was.
#
# Het script rekent zoals het capaciteitstarief: met het GEMIDDELDE van het lopende kwartier.
# Prognose = (gemiddelde tot nu × verstreken tijd + vermogen nu × resterende tijd) / 15 min.
#
# Start: Task Scheduler (zie README.md). Werkt met de gewone sh/curl van de Synology.

: "${P1:=http://192.168.0.PPP}"          # HomeWizard P1-meter
: "${HM:=http://192.168.0.HHH}"          # HeishaMon (voor commando's)
: "${HMREAD:=http://127.0.0.1:7080}"     # gegevens lezen via de warmteregelaar (valt terug op de HeishaMon)
: "${EVCC:=http://127.0.0.1:7070}"       # evcc op dezelfde Synology
: "${LIMIT:=5000}"     # W  grens voor het kwartiergemiddelde (zelfde als maxPower in evcc.yaml)
: "${MARGIN:=400}"     # W  pas terugzetten als de prognose zoveel onder de grens blijft
: "${CARIDLE:=200}"    # W  de auto telt als "gestopt" onder dit laadvermogen
: "${QUIET:=3}"        #    stille modus 1-3 (3 = stilst = minst vermogen)
: "${STAGE2:=0}"       #    1 = warmtepomp ook uitzetten als stille modus niet volstaat
: "${MAXOFF:=1200}"    # s  warmtepomp nooit langer dan 20 min uit of stil
: "${HOLD:=180}"       # s  minstens 3 min tussen twee ingrepen (spaart de compressor)
: "${SETTLE:=120}"     # s  de eerste 2 min van een kwartier niet ingrijpen: de prognose is dan nog onbetrouwbaar
: "${OVERSEC:=60}"     # s  de overschrijding moet zo lang aanhouden (korte pieken negeren)
: "${HPMIN:=500}"      # W  alleen ingrijpen als de warmtepomp minstens zoveel verbruikt
: "${ACT:=0}"          #    0 = proefperiode: alleen loggen wat het zou doen; 1 = echt ingrijpen
: "${INTERVAL:=10}"    # s
: "${LOG:=$(dirname "$0")/piekbewaker.log}"

log() { echo "$(date '+%Y-%m-%d %H:%M:%S') $*" >> "$LOG"; }
# eerste getal na "naam": in JSON (decimalen weg)
jnum() { sed -n "s/.*\"$1\": *\(-\{0,1\}[0-9][0-9]*\).*/\1/p" | head -n 1; }
# HeishaMon-waarde: {"Name":"Quiet_Mode_Level","Value":0,...}
hnum() { sed -n "s/.*\"Name\": *\"$1\", *\"Value\": *\"\{0,1\}\(-\{0,1\}[0-9][0-9]*\).*/\1/p" | head -n 1; }
send() {
  if [ "$ACT" = "1" ]; then
    curl -s -m 5 "$HM/command?$1" > /dev/null && log "→ HeishaMon $1" || log "⚠ HeishaMon niet bereikbaar ($1)"
  else
    log "(proef) zou sturen: $1"
  fi
}

hmjson() { curl -s -m 5 "$HMREAD/json" || curl -s -m 5 "$HM/json"; }

level=0          # 0 = normaal, 1 = stille modus, 2 = uit
oversince=0      # sinds wanneer de prognose boven de grens zit
noted=-1         # kwartier waarin "niets te winnen" al gelogd is
since=0          # tijdstip van de laatste ingreep
quiet0=0         # stille modus vóór de ingreep
# bij stoppen (taak opnieuw gestart, Synology uit): de warmtepomp niet stil of uit laten staan
cleanup() {
  if [ "$level" -gt 0 ]; then
    [ "$level" -eq 2 ] && send "SetHeatpump=1"
    send "SetQuietMode=$quiet0"
    log "gestopt tijdens een ingreep: warmtepomp teruggezet"
  fi
  exit 0
}
trap cleanup TERM INT HUP

log "start: grens $LIMIT W, stille modus $QUIET, uitzetten $([ "$STAGE2" = 1 ] && echo ja || echo nee), $([ "$ACT" = 1 ] && echo 'ingrijpen' || echo 'PROEF (alleen loggen)')"

while true; do
  now=$(date +%s); el=$((now % 900)); rest=$((900 - el))
  p1=$(curl -s -m 5 "$P1/api/v1/data")
  pw=$(echo "$p1" | jnum active_power_w)
  avg=$(echo "$p1" | jnum active_power_average_w)
  car=$(curl -s -m 5 "$EVCC/api/state?jq=%5B.loadpoints%5B%5D%3F%20%7C%20select(.chargerFeatureHeating%20%7C%20not)%20%7C%20.chargePower%5D%20%7C%20add" | sed 's/[^0-9.-].*//; s/\..*//')
  [ -z "$car" ] && car=0

  if [ -n "$pw" ] && [ -n "$avg" ]; then
    proj=$(( (avg * el + pw * rest) / 900 ))
    if [ "$proj" -gt "$LIMIT" ] && [ "$el" -ge "$SETTLE" ]; then
      [ "$oversince" -eq 0 ] && oversince=$now
    else
      oversince=0
    fi
    if [ "$level" -lt 2 ] && [ "$oversince" -gt 0 ] && [ $((now - oversince)) -ge "$OVERSEC" ] \
       && [ "$car" -lt "$CARIDLE" ] && [ $((now - since)) -ge "$HOLD" ]; then
      hm=$(hmjson)
      hc=$(echo "$hm" | hnum Heat_Power_Consumption); dc=$(echo "$hm" | hnum DHW_Power_Consumption)
      hpw=$(( ${hc:-0} + ${dc:-0} ))
      quarter=$((now / 900))
      if [ "$(echo "$hm" | hnum Defrosting_State)" = "1" ]; then
        :  # tijdens ontdooien niets doen
      elif [ "$level" -eq 0 ] && [ "$hpw" -lt "$HPMIN" ]; then
        if [ "$noted" -ne "$quarter" ]; then
          log "prognose kwartier ${proj} W > ${LIMIT} W (nu ${pw} W), maar de warmtepomp verbruikt maar ${hpw} W: niets te winnen bij de warmtepomp"
          noted=$quarter
        fi
      elif [ "$level" -eq 0 ]; then
        quiet0=$(echo "$hm" | hnum Quiet_Mode_Level); [ -z "$quiet0" ] && quiet0=0
        log "prognose kwartier ${proj} W > ${LIMIT} W (nu ${pw} W, warmtepomp ${hpw} W, auto ${car} W): warmtepomp stille modus $QUIET"
        send "SetQuietMode=$QUIET"; level=1; since=$now
      elif [ "$STAGE2" = "1" ]; then
        log "prognose kwartier ${proj} W > ${LIMIT} W, stille modus volstaat niet (warmtepomp ${hpw} W): warmtepomp uit"
        send "SetHeatpump=0"; level=2; since=$now
      fi
    fi
    if [ "$level" -gt 0 ] && [ "$oversince" -eq 0 ]; then
      held=$((now - since))
      # terugzetten: prognose ruim onder de grens (met de warmtepomp weer op vol vermogen erbij
      # zou het kwartier nog passen), of een nieuw kwartier is begonnen, of het duurt al te lang
      if { [ "$proj" -lt $((LIMIT - MARGIN)) ] && [ "$held" -ge "$HOLD" ]; } || [ "$el" -lt "$INTERVAL" ] || [ "$held" -ge "$MAXOFF" ]; then
        log "terugzetten (prognose ${proj} W, ${held} s ingegrepen)"
        [ "$level" -eq 2 ] && send "SetHeatpump=1"
        send "SetQuietMode=$quiet0"
        level=0; since=$now
      fi
    fi
  fi
  # logbestand klein houden
  if [ -f "$LOG" ] && [ "$(wc -l < "$LOG")" -gt 2000 ]; then tail -n 1000 "$LOG" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"; fi
  sleep "$INTERVAL" & wait $!
done
