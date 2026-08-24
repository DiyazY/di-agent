import json
import os
import threading
import time

import numpy as np
from kafka import KafkaProducer
from kafka.errors import KafkaTimeoutError

from battery import build_battery

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "localhost:9092").split(",")
KAFKA_TOPIC = os.environ.get("KAFKA_TOPIC", "battery.telemetry")
STEP_INTERVAL_S = float(os.environ.get("STEP_INTERVAL_S", "1"))
# Max load ratio change allowed per second, so the API can't force an instant jump.
RAMP_RATE_PER_S = float(os.environ.get("RAMP_RATE_PER_S", "0.05"))
# Initial state of charge (0.0-1.0) the battery starts the simulation at.
INITIAL_SOC = float(os.environ.get("INITIAL_SOC", "0.8"))


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


class BatteryController:
    """Publishes battery telemetry on a background thread and exposes a
    thread-safe API for setting the target discharge ratio at runtime.

    The battery only ever discharges onto the bus (it behaves like a genset
    from the switchboard's point of view): target_load_ratio is the fraction
    of the battery's max discharging power to deliver, ramped over time and
    capped to zero once the state of charge is depleted."""

    def __init__(self) -> None:
        self.battery = build_battery()
        self.battery_id = os.environ.get("BATTERY_ID", self.battery.name)

        self._lock = threading.Lock()
        self._target_load_ratio = 0.0
        self._current_load_ratio = 0.0
        self._soc = INITIAL_SOC
        self._last_message: dict | None = None

        self._producer: KafkaProducer | None = None
        self._stop_event = threading.Event()
        self._thread: threading.Thread | None = None

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

    def set_target_load_ratio(self, load_ratio: float) -> None:
        if not 0.0 <= load_ratio <= 1.0:
            raise ValueError("load_ratio must be between 0.0 and 1.0")
        with self._lock:
            self._target_load_ratio = load_ratio

    def get_status(self) -> dict:
        with self._lock:
            return {
                "battery_id": self.battery_id,
                "target_load_ratio": self._target_load_ratio,
                "current_load_ratio": self._current_load_ratio,
                "soc": self._soc,
                "last_message": self._last_message,
            }

    def _run(self) -> None:
        max_step = RAMP_RATE_PER_S * STEP_INTERVAL_S
        while not self._stop_event.is_set():
            with self._lock:
                target = self._target_load_ratio
                current = self._current_load_ratio
                soc = self._soc

            # An empty battery can't discharge, regardless of the requested load.
            delta = max(-max_step, min(max_step, target - current)) if soc > 0 else -max_step
            current = max(0.0, current + delta)

            # Terminal power is negative (discharging: energy leaves the battery for the bus).
            terminal_power_kw = -(self.battery.max_discharging_power_kw * current)
            power_stored_kw, load = self.battery.get_power_output_from_bidirectional_input(
                power_input=np.asarray([terminal_power_kw])
            )
            delta_kwh = float(power_stored_kw[0]) * STEP_INTERVAL_S / 3600
            soc = max(0.0, min(1.0, soc + delta_kwh / self.battery.rated_capacity_kWh))

            supply_power_kw = -terminal_power_kw if soc > 0 else 0.0
            message = {
                "battery_id": self.battery_id,
                "timestamp": time.time(),
                "load_ratio": float(load[0]),
                "power_kw": float(supply_power_kw),
                "soc": soc,
            }

            with self._lock:
                self._current_load_ratio = current
                self._soc = soc
                self._last_message = message

            self._producer.send(KAFKA_TOPIC, key=self.battery_id, value=message)
            print(
                f"load={message['load_ratio'] * 100:.1f}% "
                f"power={message['power_kw']:.1f}kW "
                f"soc={message['soc'] * 100:.1f}%"
            )

            self._stop_event.wait(STEP_INTERVAL_S)
