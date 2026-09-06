import json
import logging
import os
import threading
import time

import numpy as np
import requests
from kafka import KafkaProducer
from kafka.errors import KafkaTimeoutError

from shore_power import build_shore_power_system

logger = logging.getLogger(__name__)

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "localhost:9092").split(",")
KAFKA_TOPIC = os.environ.get("KAFKA_TOPIC", "shore_power.telemetry")
STEP_INTERVAL_S = float(os.environ.get("STEP_INTERVAL_S", "1"))
# Max power ratio change allowed per second, so the API can't force an instant jump.
RAMP_RATE_PER_S = float(os.environ.get("RAMP_RATE_PER_S", "0.1"))
RATED_POWER_KW = float(os.environ.get("RATED_POWER_KW", "1000"))
# Battery to charge while shore power is connected.
BATTERY_URL = os.environ.get("BATTERY_URL", "http://battery:8000")
BATTERY_REQUEST_TIMEOUT_S = float(os.environ.get("BATTERY_REQUEST_TIMEOUT_S", "2"))


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


class ShorePowerController:
    """Publishes shore power telemetry on a background thread and exposes a
    thread-safe API for connecting/disconnecting and setting the target power ratio.

    While connected, the delivered power (after converter losses) is forwarded to the
    battery's /charge endpoint so the battery charges from shore power instead of
    discharging onto the bus."""

    def __init__(self) -> None:
        self.shore_power_system = build_shore_power_system(rated_power_kw=RATED_POWER_KW)
        self.shore_power_id = os.environ.get("SHORE_POWER_ID", self.shore_power_system.name)

        self._lock = threading.Lock()
        self._connected = False
        self._target_power_ratio = 0.0
        self._current_power_ratio = 0.0
        self._last_message: dict | None = None

        self._producer: KafkaProducer | None = None
        self._session = requests.Session()
        self._stop_event = threading.Event()
        self._thread: threading.Thread | None = None
        self._errors: list[str] = []

    def start(self) -> None:
        self._producer = _make_producer()
        self._thread = threading.Thread(target=self._run, daemon=True)
        self._thread.start()

    def stop(self) -> None:
        self._stop_event.set()
        if self._thread is not None:
            self._thread.join(timeout=STEP_INTERVAL_S * 2)
        if self._producer is not None:
            self._producer.flush()
            self._producer.close()

    def set_connected(self, connected: bool) -> None:
        with self._lock:
            self._connected = connected

    def set_target_power_ratio(self, power_ratio: float) -> None:
        if not 0.0 <= power_ratio <= 1.0:
            raise ValueError("power_ratio must be between 0.0 and 1.0")
        with self._lock:
            self._target_power_ratio = power_ratio

    def get_status(self) -> dict:
        with self._lock:
            return {
                "shore_power_id": self.shore_power_id,
                "connected": self._connected,
                "target_power_ratio": self._target_power_ratio,
                "current_power_ratio": self._current_power_ratio,
                "last_message": self._last_message,
            }

    def get_health(self) -> dict:
        thread_status = {"telemetry": self._thread is not None and self._thread.is_alive()}
        with self._lock:
            errors = list(self._errors)
        healthy = all(thread_status.values()) and not errors
        return {"status": "ok" if healthy else "error", "threads": thread_status, "errors": errors}

    def _record_error(self, message: str) -> None:
        with self._lock:
            self._errors.append(message)

    def _send(self, topic: str, *, key: str, value: dict) -> None:
        self._producer.send(topic, key=key, value=value).get(timeout=5)

    def _charge_battery(self, power_kw: float) -> None:
        try:
            self._session.post(
                f"{BATTERY_URL}/charge",
                json={"power_kw": power_kw},
                timeout=BATTERY_REQUEST_TIMEOUT_S,
            ).raise_for_status()
        except requests.RequestException as error:
            logger.warning("Failed to forward charge request to battery: %s", error)

    def _run(self) -> None:
        try:
            self._run_loop()
        except Exception:
            logger.exception("Telemetry worker stopped unexpectedly")
            self._record_error("telemetry worker stopped unexpectedly")

    def _run_loop(self) -> None:
        max_step = RAMP_RATE_PER_S * STEP_INTERVAL_S
        converter = self.shore_power_system.converter
        while not self._stop_event.is_set():
            with self._lock:
                connected = self._connected
                target = self._target_power_ratio if connected else 0.0
                current = self._current_power_ratio

            delta = max(-max_step, min(max_step, target - current))
            current = max(0.0, current + delta)

            # Power drawn from shore, through the converter, to the bus/battery.
            input_power_kw = RATED_POWER_KW * current
            output_power_kw, load = converter.get_power_output_from_bidirectional_input(
                power_input=np.asarray([input_power_kw])
            )
            output_power_kw = float(output_power_kw[0])
            losses_kw = input_power_kw - output_power_kw

            self._charge_battery(output_power_kw)

            message = {
                "shore_power_id": self.shore_power_id,
                "timestamp": time.time(),
                "connected": connected,
                "power_ratio": float(load[0]),
                "input_power_kw": float(input_power_kw),
                "power_kw": output_power_kw,
                "losses_kw": float(losses_kw),
            }

            with self._lock:
                self._current_power_ratio = current
                self._last_message = message

            self._send(KAFKA_TOPIC, key=self.shore_power_id, value=message)
            print(
                f"connected={message['connected']} "
                f"ratio={message['power_ratio'] * 100:.1f}% "
                f"power={message['power_kw']:.1f}kW "
                f"losses={message['losses_kw']:.2f}kW"
            )

            self._stop_event.wait(STEP_INTERVAL_S)
