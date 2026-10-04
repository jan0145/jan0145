#!/usr/bin/env python3
"""GoodWe -> JSON-brug voor de Stroom-app.

Leest de GoodWe-omvormer (bv. GW8K-BT) lokaal uit via het netwerk met de
`goodwe`-bibliotheek en biedt de gegevens aan als JSON over HTTP, zodat de
Stroom-webapp ze kan ophalen.

    GET /goodwe.json  -> samenvatting + verloop van vandaag (per 5 minuten)
    GET /raw          -> alle ruwe sensorwaarden (om te controleren)

Instellen via omgevingsvariabelen:
    GOODWE_HOST   IP-adres van de GoodWe (verplicht)
    GOODWE_PORT   8899 (UDP, standaard) of 502 (Modbus/TCP)
    BRIDGE_PORT   poort van deze brug (standaard 8765)
    POLL_SECONDS  hoe vaak uitlezen (standaard 5)

Tekenafspraken in /goodwe.json:
    battery_w  + = batterij ontlaadt (levert aan huis), - = batterij laadt
    grid_w     + = afname van het net, - = teruglevering
"""

import asyncio
import json
import logging
import os
import sys
import threading
import time
from datetime import date
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import goodwe

HOST = os.environ.get("GOODWE_HOST") or (sys.argv[1] if len(sys.argv) > 1 else "")
GW_PORT = int(os.environ.get("GOODWE_PORT", "8899"))
PORT = int(os.environ.get("BRIDGE_PORT", "8765"))
INTERVAL = float(os.environ.get("POLL_SECONDS", "5"))
BUCKET = 300  # verloop bijhouden per 5 minuten

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("goodwe-bridge")

lock = threading.Lock()
state = {"ok": False, "error": "nog geen gegevens"}
raw = {}
history = {"day": None, "buckets": {}}  # bucket-start -> [som grid, som batterij, aantal]


def num(value):
    try:
        return float(value)
    except (TypeError, ValueError):
        return None


def neg(value):
    return None if value is None else (-value or 0.0)


def summarize(d):
    g = lambda key: num(d.get(key))
    return {
        "ok": True,
        "ts": time.time(),
        "battery_w": g("pbattery1"),
        "battery_soc": g("battery_soc"),
        # GoodWe telt teruglevering positief; wij draaien om naar afname = +.
        "grid_w": neg(g("active_power")),
        "grid_phases_w": [neg(g("meter_active_power1")), neg(g("meter_active_power2")), neg(g("meter_active_power3"))],
        "today_kwh": {
            "import": g("e_day_imp"),
            "export": g("e_day_exp"),
            "battery_charge": g("e_bat_charge_day"),
            "battery_discharge": g("e_bat_discharge_day"),
        },
    }


def remember(summary):
    """Houd per 5 minuten het gemiddelde bij, alleen voor vandaag."""
    if summary["grid_w"] is None or summary["battery_w"] is None:
        return
    today = date.today().isoformat()
    if history["day"] != today:
        history["day"] = today
        history["buckets"] = {}
    start = int(summary["ts"] // BUCKET * BUCKET)
    b = history["buckets"].setdefault(start, [0.0, 0.0, 0])
    b[0] += summary["grid_w"]
    b[1] += summary["battery_w"]
    b[2] += 1


def history_rows():
    return [[t, round(g / n), round(bat / n)] for t, (g, bat, n) in sorted(history["buckets"].items())]


async def poll():
    global state, raw
    inverter = None
    while True:
        try:
            if inverter is None:
                inverter = await goodwe.connect(HOST, port=GW_PORT)
                log.info("Verbonden met %s (%s, serienummer %s)", HOST, inverter.model_name, inverter.serial_number)
            data = await inverter.read_runtime_data()
            summary = summarize(data)
            summary["model"] = inverter.model_name
            with lock:
                remember(summary)
                state = summary
                raw = data
        except Exception as exc:  # netwerkfout, omvormer even weg, ...
            log.warning("Uitlezen mislukt: %s", exc)
            with lock:
                state = {"ok": False, "error": str(exc), "ts": time.time()}
            inverter = None
            await asyncio.sleep(max(INTERVAL, 15))
            continue
        await asyncio.sleep(INTERVAL)


class Handler(BaseHTTPRequestHandler):
    def _send(self, code, body):
        payload = json.dumps(body, default=str).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Private-Network", "true")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_OPTIONS(self):
        self._send(204, {})

    def do_GET(self):
        path = self.path.split("?")[0]
        with lock:
            if path in ("/", "/goodwe.json"):
                self._send(200, dict(state, history=history_rows()))
            elif path == "/raw":
                self._send(200, raw)
            else:
                self._send(404, {"error": "onbekend pad"})

    def log_message(self, *args):
        pass


def main():
    if not HOST:
        sys.exit("Geef het IP-adres van de GoodWe op: GOODWE_HOST=192.168.x.x")
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    log.info("Brug luistert op poort %d, GoodWe op %s:%d", PORT, HOST, GW_PORT)
    asyncio.run(poll())


if __name__ == "__main__":
    main()
