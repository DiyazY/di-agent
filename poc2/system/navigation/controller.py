import json
import logging
import os
import threading
import time
from datetime import datetime, timezone

import numpy as np
from kafka import KafkaProducer
from kafka.errors import KafkaTimeoutError

import nmea2000
from nmea2000 import Nmea2000Gateway
from sensors import AttitudeSensor, CompassSensor, DepthSensor, GnssSensor, WindSensor
from vessel import DEFAULT_ROUTE, VesselModel, Waypoint

logger = logging.getLogger(__name__)

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "localhost:9092").split(",")
KAFKA_TOPIC = os.environ.get("KAFKA_TOPIC", "navigation.telemetry")
# Bus tick: the virtual NMEA 2000 bus (and vessel model) updates at 5 Hz,
# matching "rapid update" PGN rates; Kafka gets 1 Hz aggregates.
TICK_INTERVAL_S = float(os.environ.get("TICK_INTERVAL_S", "0.2"))
KAFKA_EVERY_N_TICKS = int(os.environ.get("KAFKA_EVERY_N_TICKS", "5"))
# Actisense-ASCII gateway port; 2597 is the canboat n2kd convention.
N2K_HOST = os.environ.get("N2K_HOST", "0.0.0.0")
N2K_PORT = int(os.environ.get("N2K_PORT", "2597"))
VESSEL_NAME = os.environ.get("VESSEL_NAME", "vessel-1")
GNSS_ID = os.environ.get("GNSS_ID", f"{VESSEL_NAME}-gnss")
HEADING_ID = os.environ.get("HEADING_ID", f"{VESSEL_NAME}-heading")
WIND_ID = os.environ.get("WIND_ID", f"{VESSEL_NAME}-wind")
DEPTH_ID = os.environ.get("DEPTH_ID", f"{VESSEL_NAME}-depth")
ATTITUDE_ID = os.environ.get("ATTITUDE_ID", f"{VESSEL_NAME}-attitude")
INITIAL_SPEED_KNOTS = float(os.environ.get("INITIAL_SPEED_KNOTS", "8"))
# Time acceleration: 60 means one simulated minute passes per wall second, so
# the default ~5 nm route loop takes minutes instead of tens of minutes.
TIME_SCALE = float(os.environ.get("NAV_TIME_SCALE", "60"))
SENSOR_SEED = os.environ.get("SENSOR_SEED")

# NMEA 2000 source addresses for the virtual devices on the bus.
SRC_GNSS = 10
SRC_COMPASS = 20
SRC_WIND = 30
SRC_DEPTH = 40
SRC_ATTITUDE = 50

# Broadcast rates in bus ticks (tick = 0.2 s by default).
EVERY_TICK = 1          # 5 Hz: position, COG/SOG, heading, attitude
EVERY_WIND_TICKS = 3    # ~1.7 Hz: wind data
EVERY_SLOW_TICKS = 5    # 1 Hz: water depth, time & date
EVERY_LOG_TICKS = 50    # 0.1 Hz: distance log (fast-packet demo)


def _make_producer() -> KafkaProducer:
    while True:
        try:
            return KafkaProducer(
                bootstrap_servers=KAFKA_BROKERS,
                value_serializer=lambda v: json.dumps(v).encode("utf-8"),
                key_serializer=lambda k: k.encode("utf-8"),
            )
        except KafkaTimeoutError:
            print(f"Kafka brokers {KAFKA_BROKERS} not available yet, retrying in 5s ...")
            time.sleep(5)


def _make_event(source_id: str, source_type: str, payload: dict) -> dict:
    event = {
        "event": f"{source_type}.telemetry",
        "schema_version": 1,
        "source_id": source_id,
        "source_type": source_type,
        "timestamp": time.time(),
        "payload": payload,
    }
    event.update(payload)
    return event


class NavigationController:
    """Owns the vessel model, the sensor suite and the virtual NMEA 2000 bus.

    A background thread steps the vessel at TICK_INTERVAL_S, samples the
    sensors, broadcasts the encoded PGNs on the gateway, and publishes one
    Kafka event per sensor every KAFKA_EVERY_N_TICKS ticks.
    """

    def __init__(self) -> None:
        seed = int(SENSOR_SEED) if SENSOR_SEED is not None else None
        rng = np.random.default_rng(seed)
        self._vessel = VesselModel(
            route=DEFAULT_ROUTE, speed_knots=INITIAL_SPEED_KNOTS, time_scale=TIME_SCALE
        )
        self._gateway = Nmea2000Gateway(host=N2K_HOST, port=N2K_PORT)
        self._sensors = {
            "gnss": GnssSensor(rng, ref_lat=DEFAULT_ROUTE[0].lat),
            "heading": CompassSensor(rng),
            "attitude": AttitudeSensor(rng),
            "wind": WindSensor(rng),
            "depth": DepthSensor(rng),
        }
        self._sid = 0  # NMEA 2000 sequence ID, wraps at 255

        self._lock = threading.Lock()
        self._anomaly_enabled = False
        self._latest_measurements: dict = {}

        self._producer: KafkaProducer | None = None
        self._stop_event = threading.Event()
        self._thread: threading.Thread | None = None

    def start(self) -> None:
        self._producer = _make_producer()
        self._gateway.start()
        self._thread = threading.Thread(target=self._run, daemon=True)
        self._thread.start()

    def stop(self) -> None:
        self._stop_event.set()
        if self._thread is not None:
            self._thread.join(timeout=TICK_INTERVAL_S * 10)
        self._gateway.stop()
        if self._producer is not None:
            self._producer.flush()
            self._producer.close()

    def set_speed_knots(self, speed_knots: float) -> None:
        self._vessel.set_speed_knots(speed_knots)

    def set_route(self, waypoints: list[dict]) -> None:
        self._vessel.set_route([Waypoint(wp["lat"], wp["lon"]) for wp in waypoints])

    def set_anomaly_enabled(self, enabled: bool) -> None:
        with self._lock:
            self._anomaly_enabled = enabled

    def get_status(self) -> dict:
        with self._lock:
            anomaly = self._anomaly_enabled
            measurements = dict(self._latest_measurements)
        truth = self._vessel.snapshot()
        return {
            "vessel_name": VESSEL_NAME,
            "anomaly_enabled": anomaly,
            "n2k_gateway": {"port": N2K_PORT, "clients": self._gateway.client_count},
            "truth": truth,
            "measurements": measurements,
        }

    def get_latest_frames(self) -> list[str]:
        return self._gateway.latest_lines()

    def get_health(self) -> dict:
        thread_ok = self._thread is not None and self._thread.is_alive()
        return {
            "status": "ok" if thread_ok else "error",
            "telemetry_thread_alive": thread_ok,
        }

    def _run(self) -> None:
        tick = 0
        next_tick = time.monotonic()
        while not self._stop_event.is_set():
            next_tick += TICK_INTERVAL_S
            self._tick(tick)
            tick += 1
            sleep_s = next_tick - time.monotonic()
            if sleep_s > 0:
                self._stop_event.wait(sleep_s)

    def _tick(self, tick: int) -> None:
        self._vessel.step(TICK_INTERVAL_S)
        truth = self._vessel.snapshot()
        with self._lock:
            anomaly = self._anomaly_enabled
        measurements = {
            "gnss": self._sensors["gnss"].sample(truth, anomaly, TICK_INTERVAL_S),
            "heading": self._sensors["heading"].sample(truth),
            "attitude": self._sensors["attitude"].sample(truth),
            "wind": self._sensors["wind"].sample(truth),
            "depth": self._sensors["depth"].sample(truth),
        }
        with self._lock:
            self._latest_measurements = measurements
        self._sid = (self._sid + 1) % 256
        self._broadcast(tick, truth, measurements)
        if tick % KAFKA_EVERY_N_TICKS == 0:
            self._publish_kafka(measurements)

    def _broadcast(self, tick: int, truth: dict, measurements: dict) -> None:
        sid = self._sid
        gw = self._gateway
        gnss = measurements["gnss"]
        heading = measurements["heading"]
        attitude = measurements["attitude"]
        wind = measurements["wind"]
        depth = measurements["depth"]

        if tick % EVERY_TICK == 0:
            gw.broadcast(2, nmea2000.PGN_POSITION_RAPID, SRC_GNSS,
                         nmea2000.encode_position_rapid(gnss["latitude_deg"], gnss["longitude_deg"]))
            gw.broadcast(2, nmea2000.PGN_COG_SOG_RAPID, SRC_GNSS,
                         nmea2000.encode_cog_sog_rapid(
                             sid, gnss["cog_deg"], gnss["sog_knots"] * nmea2000.KNOTS_TO_M_S))
            gw.broadcast(2, nmea2000.PGN_VESSEL_HEADING, SRC_COMPASS,
                         nmea2000.encode_heading(sid, heading["heading_deg"]))
            gw.broadcast(2, nmea2000.PGN_ATTITUDE, SRC_ATTITUDE,
                         nmea2000.encode_attitude(
                             sid, attitude["yaw_deg"], attitude["pitch_deg"], attitude["roll_deg"]))
        if tick % EVERY_WIND_TICKS == 0:
            gw.broadcast(2, nmea2000.PGN_WIND_DATA, SRC_WIND,
                         nmea2000.encode_wind(sid, wind["wind_speed_m_s"], wind["wind_angle_deg"]))
        if tick % EVERY_SLOW_TICKS == 0:
            gw.broadcast(3, nmea2000.PGN_WATER_DEPTH, SRC_DEPTH,
                         nmea2000.encode_water_depth(sid, depth["depth_m"], depth["offset_m"]))
            gw.broadcast(2, nmea2000.PGN_RATE_OF_TURN, SRC_COMPASS,
                         nmea2000.encode_rate_of_turn(sid, heading["rot_deg_per_min"]))
            days, seconds = _utc_days_and_seconds()
            gw.broadcast(3, nmea2000.PGN_TIME_DATE, SRC_GNSS,
                         nmea2000.encode_time_date(days, seconds))
        if tick % EVERY_LOG_TICKS == 0:
            days, seconds = _utc_days_and_seconds()
            gw.broadcast(6, nmea2000.PGN_DISTANCE_LOG, SRC_GNSS,
                         nmea2000.encode_distance_log(
                             days, seconds, truth["total_log_m"], truth["trip_log_m"]))

    def _publish_kafka(self, measurements: dict) -> None:
        assert self._producer is not None
        events = (
            _make_event(GNSS_ID, "navigation", {"gnss_id": GNSS_ID, "vessel": VESSEL_NAME,
                                                **measurements["gnss"]}),
            _make_event(HEADING_ID, "navigation", {"heading_id": HEADING_ID, "vessel": VESSEL_NAME,
                                                   **measurements["heading"]}),
            _make_event(ATTITUDE_ID, "navigation", {"attitude_id": ATTITUDE_ID, "vessel": VESSEL_NAME,
                                                    **measurements["attitude"]}),
            _make_event(WIND_ID, "navigation", {"wind_id": WIND_ID, "vessel": VESSEL_NAME,
                                                **{
                                                    k: v
                                                    for k, v in measurements["wind"].items()
                                                    if not k.startswith("true_")
                                                }}),
            _make_event(DEPTH_ID, "navigation", {"depth_id": DEPTH_ID, "vessel": VESSEL_NAME,
                                                 **measurements["depth"]}),
        )
        for event in events:
            self._producer.send(KAFKA_TOPIC, key=event["source_id"], value=event)


def _utc_days_and_seconds() -> tuple[int, float]:
    now = datetime.now(timezone.utc)
    midnight = now.replace(hour=0, minute=0, second=0, microsecond=0)
    days = (now.date() - datetime(1970, 1, 1, tzinfo=timezone.utc).date()).days
    return days, (now - midnight).total_seconds()
