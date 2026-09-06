import json
import logging
import os
import threading
import time

import numpy as np
from kafka import KafkaProducer
from kafka.errors import KafkaTimeoutError

from battery import build_battery

logger = logging.getLogger(__name__)

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "localhost:9092").split(",")
KAFKA_TOPIC = os.environ.get("KAFKA_TOPIC", "battery.telemetry")
STEP_INTERVAL_S = float(os.environ.get("STEP_INTERVAL_S", "1"))
# Max load ratio change allowed per second, so the API can't force an instant jump.
RAMP_RATE_PER_S = float(os.environ.get("RAMP_RATE_PER_S", "0.05"))
# Initial state of charge (0.0-1.0) the battery starts the simulation at.
INITIAL_SOC = float(os.environ.get("INITIAL_SOC", "0.8"))
# Smoothing factor for the net power EMA used to predict time-to-empty/time-to-full;
# lower values smooth out load ramp transients more but react slower to real changes.
PREDICTION_EMA_ALPHA = float(os.environ.get("PREDICTION_EMA_ALPHA", "0.1"))
# Below this |SoC change per hour| the trend is considered flat (no prediction).
PREDICTION_MIN_SOC_RATE_PER_HR = float(os.environ.get("PREDICTION_MIN_SOC_RATE_PER_HR", "0.001"))


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


class BatteryController:
    """Publishes battery telemetry on a background thread and exposes a
    thread-safe API for setting the target discharge ratio and charge power at runtime.

    The battery discharges onto the bus (it behaves like a genset from the switchboard's
    point of view) via target_load_ratio: the fraction of the battery's max discharging
    power to deliver, ramped over time and capped to zero once depleted.

    It can also be charged (e.g. by a shore power connection) via target_charge_power_kw:
    an absolute power in kW that the battery accepts, clamped to its own max charging power
    and ramped down to zero once full. Charging and discharging are netted at the terminal,
    matching FEEMS' Battery sign convention (positive terminal power = charging)."""

    def __init__(self) -> None:
        self.battery = build_battery()
        self.battery_id = os.environ.get("BATTERY_ID", self.battery.name)

        self._lock = threading.Lock()
        self._target_load_ratio = 0.0
        self._current_load_ratio = 0.0
        self._target_charge_power_kw = 0.0
        self._current_charge_power_kw = 0.0
        self._soc = INITIAL_SOC
        self._power_stored_ema_kw = 0.0
        self._last_message: dict | None = None

        self._producer: KafkaProducer | None = None
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

    def set_target_load_ratio(self, load_ratio: float) -> None:
        if not 0.0 <= load_ratio <= 1.0:
            raise ValueError("load_ratio must be between 0.0 and 1.0")
        with self._lock:
            self._target_load_ratio = load_ratio

    def set_target_charge_power_kw(self, power_kw: float) -> None:
        if power_kw < 0.0:
            raise ValueError("power_kw must be >= 0.0")
        with self._lock:
            self._target_charge_power_kw = power_kw

    def get_status(self) -> dict:
        with self._lock:
            return {
                "battery_id": self.battery_id,
                "target_load_ratio": self._target_load_ratio,
                "current_load_ratio": self._current_load_ratio,
                "target_charge_power_kw": self._target_charge_power_kw,
                "current_charge_power_kw": self._current_charge_power_kw,
                "max_charging_power_kw": self.battery.max_charging_power_kw,
                "soc": self._soc,
                **self._predict_locked(),
                "last_message": self._last_message,
            }

    def _predict_locked(self) -> dict:
        """Predicts time-to-empty/time-to-full from the smoothed net power flow.
        Must be called with self._lock held."""
        soc_rate_per_hour = self._power_stored_ema_kw / self.battery.rated_capacity_kWh
        prediction = {"soc_rate_per_hour": soc_rate_per_hour}
        if soc_rate_per_hour <= -PREDICTION_MIN_SOC_RATE_PER_HR:
            prediction["time_to_empty_hr"] = self._soc / -soc_rate_per_hour
        elif soc_rate_per_hour >= PREDICTION_MIN_SOC_RATE_PER_HR:
            prediction["time_to_full_hr"] = (1.0 - self._soc) / soc_rate_per_hour
        return prediction

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

    def _run(self) -> None:
        try:
            self._run_loop()
        except Exception:
            logger.exception("Telemetry worker stopped unexpectedly")
            self._record_error("telemetry worker stopped unexpectedly")

    def _run_loop(self) -> None:
        max_step = RAMP_RATE_PER_S * STEP_INTERVAL_S
        max_charging_power_kw = self.battery.max_charging_power_kw
        max_charge_step_kw = RAMP_RATE_PER_S * max_charging_power_kw * STEP_INTERVAL_S
        while not self._stop_event.is_set():
            with self._lock:
                target = self._target_load_ratio
                current = self._current_load_ratio
                target_charge_kw = self._target_charge_power_kw
                current_charge_kw = self._current_charge_power_kw
                soc = self._soc

            # An empty battery can't discharge, regardless of the requested load.
            delta = max(-max_step, min(max_step, target - current)) if soc > 0 else -max_step
            current = max(0.0, current + delta)

            # A full battery can't accept more charge, regardless of what's offered.
            charge_target = min(target_charge_kw, max_charging_power_kw) if soc < 1.0 else 0.0
            charge_delta = max(
                -max_charge_step_kw, min(max_charge_step_kw, charge_target - current_charge_kw)
            )
            current_charge_kw = max(0.0, current_charge_kw + charge_delta)

            # Terminal power nets charging (positive) and discharging (negative), matching
            # FEEMS' Battery sign convention (positive power_input = charging).
            terminal_power_kw = current_charge_kw - (self.battery.max_discharging_power_kw * current)
            power_stored_kw, load = self.battery.get_power_output_from_bidirectional_input(
                power_input=np.asarray([terminal_power_kw])
            )
            delta_kwh = float(power_stored_kw[0]) * STEP_INTERVAL_S / 3600
            soc = max(0.0, min(1.0, soc + delta_kwh / self.battery.rated_capacity_kWh))

            # EMA of net stored power so ramp transients don't make the prediction noisy.
            power_stored_ema_kw = (
                PREDICTION_EMA_ALPHA * float(power_stored_kw[0])
                + (1 - PREDICTION_EMA_ALPHA) * self._power_stored_ema_kw
            )

            # Bus-side power: positive when the battery supplies the bus (discharging),
            # negative when it draws from the bus (charging).
            supply_power_kw = -terminal_power_kw
            message = _make_event(
                self.battery_id,
                "battery",
                {
                    "battery_id": self.battery_id,
                    "timestamp": time.time(),
                    "load_ratio": float(load[0]),
                    "power_kw": float(supply_power_kw),
                    "charge_power_kw": float(current_charge_kw),
                    "soc": soc,
                },
            )

            with self._lock:
                self._current_load_ratio = current
                self._current_charge_power_kw = current_charge_kw
                self._soc = soc
                self._power_stored_ema_kw = power_stored_ema_kw
                prediction = self._predict_locked()
                message["payload"].update(prediction)
                # Mirrored into the top-level event, matching _make_event's
                # flattening, so the log line below can read the prediction keys.
                message.update(prediction)
                self._last_message = message

            self._send(KAFKA_TOPIC, key=self.battery_id, value=message)
            prediction_text = (
                f"time_to_empty={message['time_to_empty_hr']:.2f}h"
                if "time_to_empty_hr" in message
                else (
                    f"time_to_full={message['time_to_full_hr']:.2f}h"
                    if "time_to_full_hr" in message
                    else "stable"
                )
            )
            print(
                f"load={message['load_ratio'] * 100:.1f}% "
                f"power={message['power_kw']:.1f}kW "
                f"charge={message['charge_power_kw']:.1f}kW "
                f"soc={message['soc'] * 100:.1f}% "
                f"{prediction_text}"
            )

            self._stop_event.wait(STEP_INTERVAL_S)
