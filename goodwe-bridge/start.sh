#!/bin/sh
# Start de GoodWe-brug zonder Docker (bv. via Taakplanner op een Synology).
# IP-adres van de GoodWe: hieronder aanpassen, of meegeven als GOODWE_HOST=... sh start.sh
GOODWE_HOST="${GOODWE_HOST:-192.168.1.100}"

PATH="/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"
cd "$(dirname "$0")" || exit 1

# Python zoeken: eerst in het pad, dan op de plaatsen waar Synology-pakketten het zetten.
PY=""
for p in python3 /usr/local/bin/python3 /usr/bin/python3 /bin/python3 \
         /var/packages/Python3*/target/usr/bin/python3* /usr/local/bin/python3.*; do
  if command -v "$p" >/dev/null 2>&1 && "$p" -c 'import sys; sys.exit(sys.version_info < (3, 8))' 2>/dev/null; then
    PY="$p"; break
  fi
done
if [ -z "$PY" ]; then
  echo "$(date) Geen Python 3.8 of nieuwer gevonden. Installeer 'Python 3' via het Package Center." > bridge.log
  exit 1
fi

# Een eventueel draaiende brug eerst stoppen.
[ -f bridge.pid ] && kill "$(cat bridge.pid)" 2>/dev/null && sleep 2
export GOODWE_HOST GOODWE_PORT="${GOODWE_PORT:-8899}" BRIDGE_PORT="${BRIDGE_PORT:-8765}"
echo "$(date) Start met $PY" > bridge.log
nohup "$PY" -u bridge.py >> bridge.log 2>&1 &
echo $! > bridge.pid
