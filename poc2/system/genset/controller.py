import json
import logging
import os
import threading
import time

import numpy as np
from feems.types_for_feems import EmissionType
from kafka import KafkaProducer
from kafka.errors import KafkaTimeoutError

from genset import build_genset
from sensors import NUM_CYLINDERS, SensorSimulator

logger = logging.getLogger(__name__)

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "localhost:9092").split(",")
KAFKA_TOPIC = os.environ.get("KAFKA_TOPIC", "genset.telemetry")
STEP_INTERVAL_S = float(os.environ.get("STEP_INTERVAL_S", "1"))
# Ramp rate is piecewise: 0-50% load ramps faster than 50-100%, so that reaching 50%
# load takes ~50s and reaching 100% load takes ~190s total, matching typical genset
# loading guidance (fast up to half load, slower beyond).
RAMP_RATE_LOW_PER_S = float(os.environ.get("RAMP_RATE_LOW_PER_S", str(0.5))) # 0.5 / 50
RAMP_RATE_HIGH_PER_S = float(os.environ.get("RAMP_RATE_HIGH_PER_S", str(0.178))) # 0.5 / 140
RAMP_SWITCH_LOAD_RATIO = float(os.environ.get("RAMP_SWITCH_LOAD_RATIO", "0.5"))
# Unloading is a single constant rate so that 100% -> 0% always takes ~45s.
RAMP_RATE_DOWN_PER_S = float(os.environ.get("RAMP_RATE_DOWN_PER_S", str(1.0))) # 1.0 / 45
# Seed for the Gaussian sensor simulator; unset gives a fresh random sequence per start.
SENSOR_SEED = os.environ.get("SENSOR_SEED")


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


class GensetController:
    """Publishes genset telemetry on a background thread and exposes a
    thread-safe API for setting the target load ratio at runtime."""

    def __init__(self) -> None:
        self.genset = build_genset()
        self.genset_id = os.environ.get("GENSET_ID", self.genset.name)
        self._sensors = SensorSimulator(seed=int(SENSOR_SEED) if SENSOR_SEED is not None else None)

        self._lock = threading.Lock()
        self._target_load_ratio = 0.0
        self._current_load_ratio = 0.0
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

    def get_status(self) -> dict:
        with self._lock:
            return {
                "genset_id": self.genset_id,
                "target_load_ratio": self._target_load_ratio,
                "current_load_ratio": self._current_load_ratio,
                "speed_rpm": self.genset.rated_speed * self._current_load_ratio,
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

    def _run(self) -> None:
        try:
            self._run_loop()
        except Exception:
            logger.exception("Telemetry worker stopped unexpectedly")
            self._record_error("telemetry worker stopped unexpectedly")

    def _run_loop(self) -> None:
        while not self._stop_event.is_set():
            with self._lock:
                target = self._target_load_ratio
                current = self._current_load_ratio

            rate = (
                RAMP_RATE_DOWN_PER_S
                if target < current
                else (
                    RAMP_RATE_LOW_PER_S
                    if current < RAMP_SWITCH_LOAD_RATIO
                    else RAMP_RATE_HIGH_PER_S
                )
            )
            max_step = rate * STEP_INTERVAL_S
            delta = max(-max_step, min(max_step, target - current))
            current += delta
            # Genset.rated_power is the generator's rated power (electric output side), so the
            # load ratio is relative to the electric output and the generator stays in the loop.
            power_kw = self.genset.rated_power * current

            # Run the full genset chain: electric power -> generator efficiency curve ->
            # engine shaft power -> engine run point (fuel, bsfc, emissions).
            run_point = self.genset.get_fuel_cons_load_bsfc_from_power_out_generator_kw(
                power=np.asarray([power_kw])
            )
            engine_run_point = run_point.engine
            fuel_flow_kg_per_s = float(
                np.atleast_1d(engine_run_point.fuel_flow_rate_kg_per_s.total_fuel_consumption)[0]
            )
            # Tank-to-wake CO2 from combustion, derived from the fuel's GHG factor table.
            # get_total_co2_emissions() returns an ndarray of GHGEmissions (one per power_kw entry).
            co2_emissions = engine_run_point.fuel_flow_rate_kg_per_s.get_total_co2_emissions()[0]
            co2_kg_per_s = float(
                np.atleast_1d(co2_emissions.tank_to_wake_kg_or_gco2eq_per_gfuel)[0]
            )
            nox_kg_per_s = float(
                np.atleast_1d(engine_run_point.emissions_g_per_s.get(EmissionType.NOX, 0.0))[0] / 1000
            )
            load_ratio = float(run_point.genset_load_ratio[0])
            speed_rpm = self.genset.rated_speed * load_ratio
            message = _make_event(
                self.genset_id,
                "genset",
                {
                    "genset_id": self.genset_id,
                    "timestamp": time.time(),
                    "load_ratio": load_ratio,
                    "power_kw": float(power_kw),
                    "speed_rpm": speed_rpm,
                    "fuel_flow_kg_per_s": fuel_flow_kg_per_s,
                    "bsfc_g_per_kwh": float(engine_run_point.bsfc_g_per_kWh[0]),
                    "co2_kg_per_s": co2_kg_per_s,
                    "nox_kg_per_s": nox_kg_per_s,
                },
            )
            # Gaussian auxiliary sensors (ambient, lube oil, vibration, per-cylinder).
            # Mirrored into the top-level event, matching _make_event's flattening,
            # so the log line below (and any flat-schema consumer) can read them.
            sensor_values = self._sensors.simulate(load_ratio)
            message["payload"].update(sensor_values)
            message.update(sensor_values)

            with self._lock:
                self._current_load_ratio = current
                self._last_message = message

            self._send(KAFKA_TOPIC, key=self.genset_id, value=message)
            print(
                f"load={message['load_ratio'] * 100:.1f}% "
                f"power={message['power_kw']:.1f}kW "
                f"speed_rpm={message['speed_rpm']:.1f}rpm "
                f"fuel_flow={message['fuel_flow_kg_per_s']:.5f}kg/s "
                f"bsfc={message['bsfc_g_per_kwh']:.1f}g/kWh "
                f"co2={message['co2_kg_per_s']:.5f}kg/s "
                f"nox={message['nox_kg_per_s']:.6f}kg/s "
                f"oil_temp={message['oil_temp_c']:.1f}C "
                f"oil_press={message['oil_pressure_bar']:.2f}bar "
                f"exh_temp_avg={np.mean([message[f'cylinder_exhaust_temp_c_{i}'] for i in range(1, NUM_CYLINDERS + 1)]):.1f}C"
            )

            self._stop_event.wait(STEP_INTERVAL_S)
