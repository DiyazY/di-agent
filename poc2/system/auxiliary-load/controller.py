import json
import logging
import os
import threading
import time

from kafka import KafkaConsumer, KafkaProducer
from kafka.errors import KafkaTimeoutError

from aux_load import build_auxiliary_load

logger = logging.getLogger(__name__)

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "localhost:9092").split(",")
KAFKA_TOPIC = os.environ.get("KAFKA_TOPIC", "auxload.telemetry")
STEP_INTERVAL_S = float(os.environ.get("STEP_INTERVAL_S", "1"))
# Max load ratio change allowed per second, so the API can't force an instant jump.
RAMP_RATE_PER_S = float(os.environ.get("RAMP_RATE_PER_S", "0.05"))
# Initial target load ratio (hotel loads such as HVAC/lighting/galley
# typically start around a medium level, but this is adjustable via the API).
INITIAL_LOAD_RATIO = float(os.environ.get("INITIAL_LOAD_RATIO", "0.5"))

REQUEST_KAFKA_TOPIC = os.environ.get("REQUEST_KAFKA_TOPIC", "switchboard.requests")
ALLOCATION_KAFKA_TOPIC = os.environ.get("ALLOCATION_KAFKA_TOPIC", "switchboard.telemetry")
# Load-shedding priority: consumers with a higher value are served first by
# the switchboard when supply can't cover total demand.
AUXLOAD_PRIORITY = int(os.environ.get("AUXLOAD_PRIORITY", "1"))
# An allocation is considered stale (treated as 0 kW available) if none was
# received within this many seconds, e.g. the switchboard is down.
ALLOCATION_STALE_TIMEOUT_S = float(os.environ.get("ALLOCATION_STALE_TIMEOUT_S", "5"))


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


def _make_consumer() -> KafkaConsumer:
    while True:
        try:
            return KafkaConsumer(
                ALLOCATION_KAFKA_TOPIC,
                bootstrap_servers=KAFKA_BROKERS,
                value_deserializer=lambda v: json.loads(v.decode("utf-8")),
                auto_offset_reset="latest",
                group_id=None,
            )
        except KafkaTimeoutError:
            print(f"Kafka brokers {KAFKA_BROKERS} not available yet, retrying in 5s ...")
            time.sleep(5)


class AuxLoadController:
    """Publishes auxiliary (hotel) load telemetry on a background thread and
    exposes a thread-safe API for setting the target load ratio at runtime.

    It ramps toward the target load ratio, capped to whatever the
    switchboard actually allocates back to it, same as propulsion."""

    def __init__(self) -> None:
        self.aux_load = build_auxiliary_load()
        self.auxload_id = os.environ.get("AUXLOAD_ID", self.aux_load.name)

        self._lock = threading.Lock()
        self._target_load_ratio = INITIAL_LOAD_RATIO
        self._ramped_load_ratio = 0.0
        self._achieved_load_ratio = 0.0
        self._last_message: dict | None = None
        self._allocation: tuple[float, float] | None = None

        self._producer: KafkaProducer | None = None
        self._consumer: KafkaConsumer | None = None
        self._stop_event = threading.Event()
        self._thread: threading.Thread | None = None
        self._consumer_thread: threading.Thread | None = None
        self._errors: list[str] = []

    def start(self) -> None:
        self._producer = _make_producer()
        self._consumer = _make_consumer()
        self._consumer_thread = threading.Thread(target=self._consume_allocations, daemon=True)
        self._consumer_thread.start()
        self._thread = threading.Thread(target=self._run, daemon=True)
        self._thread.start()

    def stop(self) -> None:
        self._stop_event.set()
        if self._consumer is not None:
            self._consumer.close()
        if self._thread is not None:
            self._thread.join(timeout=STEP_INTERVAL_S * 2)
        if self._consumer_thread is not None:
            self._consumer_thread.join(timeout=STEP_INTERVAL_S * 2)
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
                "auxload_id": self.auxload_id,
                "target_load_ratio": self._target_load_ratio,
                "current_load_ratio": self._achieved_load_ratio,
                "allocated_power_kw": self._get_allocated_power_kw(),
                "last_message": self._last_message,
            }

    def get_health(self) -> dict:
        threads = {
            "telemetry": self._thread,
            "allocation_consumer": self._consumer_thread,
        }
        with self._lock:
            errors = list(self._errors)
        thread_status = {
            name: thread is not None and thread.is_alive() for name, thread in threads.items()
        }
        healthy = all(thread_status.values()) and not errors
        return {"status": "ok" if healthy else "error", "threads": thread_status, "errors": errors}

    def _record_error(self, message: str) -> None:
        with self._lock:
            self._errors.append(message)

    def _send(self, topic: str, *, key: str, value: dict) -> None:
        self._producer.send(topic, key=key, value=value).get(timeout=5)

    def _consume_allocations(self) -> None:
        try:
            for record in self._consumer:
                if self._stop_event.is_set():
                    break
                value = record.value
                if value.get("consumer_id") != self.auxload_id:
                    continue
                allocated_power_kw = value.get("allocated_power_kw")
                if allocated_power_kw is None:
                    continue
                with self._lock:
                    self._allocation = (float(allocated_power_kw), time.time())
        except Exception:
            if not self._stop_event.is_set():
                logger.exception("Allocation consumer stopped unexpectedly")
                self._record_error("allocation_consumer stopped unexpectedly")

    def _get_allocated_power_kw(self) -> float:
        """0 kW if no allocation was ever received, or the last one is stale
        (e.g. the switchboard is down). Must be called with self._lock held."""
        if self._allocation is None:
            return 0.0
        allocated_power_kw, received_at = self._allocation
        if time.time() - received_at > ALLOCATION_STALE_TIMEOUT_S:
            return 0.0
        return allocated_power_kw

    def _run(self) -> None:
        try:
            self._run_loop()
        except Exception:
            logger.exception("Telemetry worker stopped unexpectedly")
            self._record_error("telemetry worker stopped unexpectedly")

    def _run_loop(self) -> None:
        max_step = RAMP_RATE_PER_S * STEP_INTERVAL_S
        while not self._stop_event.is_set():
            with self._lock:
                target = self._target_load_ratio
                current = self._ramped_load_ratio
                allocated_power_kw = self._get_allocated_power_kw()

            delta = max(-max_step, min(max_step, target - current))
            current += delta
            desired_power_output_kw = self.aux_load.rated_power * current
            desired_power_input_kw, _ = self.aux_load.get_power_input_from_bidirectional_output(
                desired_power_output_kw
            )

            self._send(
                REQUEST_KAFKA_TOPIC,
                key=self.auxload_id,
                value={
                    "consumer_id": self.auxload_id,
                    "timestamp": time.time(),
                    "requested_power_kw": float(desired_power_input_kw),
                    "priority": AUXLOAD_PRIORITY,
                },
            )

            power_input_kw = min(desired_power_input_kw, allocated_power_kw)
            power_output_kw, load_ratio = self.aux_load.get_power_output_from_bidirectional_input(
                power_input_kw
            )

            message = {
                "auxload_id": self.auxload_id,
                "timestamp": time.time(),
                "load_ratio": float(load_ratio),
                "power_output_kw": float(power_output_kw),
                "power_input_kw": float(power_input_kw),
                "allocated_power_kw": float(allocated_power_kw),
            }

            with self._lock:
                self._ramped_load_ratio = current
                self._achieved_load_ratio = float(load_ratio)
                self._last_message = message

            self._send(KAFKA_TOPIC, key=self.auxload_id, value=message)
            print(
                f"load={message['load_ratio'] * 100:.1f}% "
                f"power_output={message['power_output_kw']:.1f}kW "
                f"allocated={message['allocated_power_kw']:.1f}kW"
            )

            self._stop_event.wait(STEP_INTERVAL_S)
