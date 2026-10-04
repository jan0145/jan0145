#!/bin/sh
# Start de GoodWe-brug zonder Docker (bv. via Taakplanner op een Synology).
# IP-adres van de GoodWe: hieronder aanpassen, of meegeven als GOODWE_HOST=... sh start.sh
GOODWE_HOST="${GOODWE_HOST:-192.168.1.100}"

cd "$(dirname "$0")"
# Een eventueel draaiende brug eerst stoppen.
[ -f bridge.pid ] && kill "$(cat bridge.pid)" 2>/dev/null && sleep 2
export GOODWE_HOST GOODWE_PORT="${GOODWE_PORT:-8899}" BRIDGE_PORT="${BRIDGE_PORT:-8765}"
nohup python3 -u bridge.py > bridge.log 2>&1 &
echo $! > bridge.pid
