import json
import os
import threading
import time

from kafka import KafkaProducer
from kafka.errors import KafkaTimeoutError

from propulsion import build_propulsion_drive

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "localhost:9092").split(",")
KAFKA_TOPIC = os.environ.get("KAFKA_TOPIC", "propulsion.telemetry")
STEP_INTERVAL_S = float(os.environ.get("STEP_INTERVAL_S", "1"))
# Max load ratio change allowed per second, so the API can't force an instant jump.
RAMP_RATE_PER_S = float(os.environ.get("RAMP_RATE_PER_S", "0.05"))


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


class PropulsionController:
    """Publishes propulsion drive telemetry on a background thread and exposes a
    thread-safe API for setting the target load ratio at runtime."""

    def __init__(self) -> None:
        self.propulsion_drive = build_propulsion_drive()
        self.propulsion_id = os.environ.get("PROPULSION_ID", self.propulsion_drive.name)

        self._lock = threading.Lock()
        self._target_load_ratio = 0.0
        self._current_load_ratio = 0.0
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
                "propulsion_id": self.propulsion_id,
                "target_load_ratio": self._target_load_ratio,
                "current_load_ratio": self._current_load_ratio,
                "last_message": self._last_message,
            }

    def _run(self) -> None:
        max_step = RAMP_RATE_PER_S * STEP_INTERVAL_S
        while not self._stop_event.is_set():
            with self._lock:
                target = self._target_load_ratio
                current = self._current_load_ratio

            delta = max(-max_step, min(max_step, target - current))
            current += delta
            power_output_kw = self.propulsion_drive.rated_power * current

            power_input_kw, load_ratio = self.propulsion_drive.get_power_input_from_bidirectional_output(
                power_output_kw
            )

            message = {
                "propulsion_id": self.propulsion_id,
                "timestamp": time.time(),
                "load_ratio": float(load_ratio),
                "power_output_kw": float(power_output_kw),
                "power_input_kw": float(power_input_kw),
            }

            with self._lock:
                self._current_load_ratio = current
                self._last_message = message

            self._producer.send(KAFKA_TOPIC, key=self.propulsion_id, value=message)
            print(
                f"load={message['load_ratio'] * 100:.1f}% "
                f"power_output={message['power_output_kw']:.1f}kW "
                f"power_input={message['power_input_kw']:.1f}kW"
            )

            self._stop_event.wait(STEP_INTERVAL_S)
