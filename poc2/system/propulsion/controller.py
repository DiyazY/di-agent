import json
import logging
import os
import threading
import time

from kafka import KafkaConsumer, KafkaProducer
from kafka.errors import KafkaTimeoutError

from propulsion import build_propulsion_drive

logger = logging.getLogger(__name__)

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "localhost:9092").split(",")
KAFKA_TOPIC = os.environ.get("KAFKA_TOPIC", "propulsion.telemetry")
STEP_INTERVAL_S = float(os.environ.get("STEP_INTERVAL_S", "1"))
# Max load ratio change allowed per second, so the API can't force an instant jump.
RAMP_RATE_PER_S = float(os.environ.get("RAMP_RATE_PER_S", "0.05"))

# The propulsion drive no longer reads genset telemetry directly: it asks the
# switchboard for power and only draws what the switchboard grants it. This
# lets any number of gensets and consumers share one bus without every
# consumer having to sum genset telemetry itself.
REQUEST_KAFKA_TOPIC = os.environ.get("REQUEST_KAFKA_TOPIC", "switchboard.requests")
ALLOCATION_KAFKA_TOPIC = os.environ.get("ALLOCATION_KAFKA_TOPIC", "switchboard.telemetry")
# Load-shedding priority: consumers with a higher value are served first by
# the switchboard when supply can't cover total demand.
PROPULSION_PRIORITY = int(os.environ.get("PROPULSION_PRIORITY", "2"))
# An allocation is considered stale (treated as 0 kW available) if none was
# received within this many seconds, e.g. the switchboard is down.
ALLOCATION_STALE_TIMEOUT_S = float(os.environ.get("ALLOCATION_STALE_TIMEOUT_S", "5"))
# Bisection tolerance (kW) used to find the max power output the drive can
# take without its power input exceeding the allocated power.
POWER_INPUT_LIMIT_TOLERANCE_KW = float(os.environ.get("POWER_INPUT_LIMIT_TOLERANCE_KW", "0.1"))


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


class PropulsionController:
    """Publishes propulsion drive telemetry on a background thread and exposes a
    thread-safe API for setting the target load ratio at runtime.

    Each tick it tells the switchboard how much power it wants, then caps its
    own power output to whatever the switchboard actually allocated back to
    it, so it never draws more than the bus can currently supply."""

    def __init__(self) -> None:
        self.propulsion_drive = build_propulsion_drive()
        self.propulsion_id = os.environ.get("PROPULSION_ID", self.propulsion_drive.name)

        self._lock = threading.Lock()
        self._target_load_ratio = 0.0
        # Ramped setpoint the controller is chasing, ignoring the switchboard
        # allocation; used only to keep ramping continuous, never reported directly.
        self._ramped_target_load_ratio = 0.0
        # Actual load ratio achieved after capping power_output_kw to what the
        # switchboard allocated; this is what's reported as current_load_ratio.
        self._achieved_load_ratio = 0.0
        self._last_message: dict | None = None
        # (allocated_power_kw, received_at) for this consumer's own id.
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
                "propulsion_id": self.propulsion_id,
                "target_load_ratio": self._target_load_ratio,
                "current_load_ratio": self._achieved_load_ratio,
                "allocated_power_kw": self._get_allocated_power_kw(),
                "speed_rpm": self._speed_rpm(self._achieved_load_ratio),
                "last_message": self._last_message,
            }

    def _speed_rpm(self, load_ratio: float) -> float:
        """Propeller-law estimate of shaft speed: for a fixed-pitch propeller
        power varies with the cube of rpm, so speed varies with the cube root
        of the load ratio."""
        return self.propulsion_drive.rated_speed * load_ratio

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
        return {
            "status": "ok" if healthy else "error",
            "threads": thread_status,
            "errors": errors,
        }

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
                if value.get("consumer_id") != self.propulsion_id:
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

    def _max_power_output_for_available_power(self, available_power_kw: float) -> float:
        """Largest power output the drive can produce without its power input
        exceeding available_power_kw, found by bisection since power input is
        monotonically increasing with power output."""
        if available_power_kw <= 0:
            return 0.0

        low, high = 0.0, self.propulsion_drive.rated_power
        power_input_at_high, _ = self.propulsion_drive.get_power_input_from_bidirectional_output(high)
        if power_input_at_high <= available_power_kw:
            return high

        while high - low > POWER_INPUT_LIMIT_TOLERANCE_KW:
            mid = (low + high) / 2
            power_input_mid, _ = self.propulsion_drive.get_power_input_from_bidirectional_output(mid)
            if power_input_mid <= available_power_kw:
                low = mid
            else:
                high = mid
        return low

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
                current = self._ramped_target_load_ratio
                allocated_power_kw = self._get_allocated_power_kw()

            delta = max(-max_step, min(max_step, target - current))
            current += delta
            desired_power_output_kw = self.propulsion_drive.rated_power * current
            desired_power_input_kw, _ = self.propulsion_drive.get_power_input_from_bidirectional_output(
                desired_power_output_kw
            )

            # Tell the switchboard how much power this tick's target would
            # need, regardless of what was allocated last tick.
            self._send(
                REQUEST_KAFKA_TOPIC,
                key=self.propulsion_id,
                value={
                    "consumer_id": self.propulsion_id,
                    "timestamp": time.time(),
                    "requested_power_kw": float(desired_power_input_kw),
                    "priority": PROPULSION_PRIORITY,
                },
            )

            max_power_output_kw = self._max_power_output_for_available_power(allocated_power_kw)
            power_output_kw = min(desired_power_output_kw, max_power_output_kw)

            power_input_kw, load_ratio = self.propulsion_drive.get_power_input_from_bidirectional_output(
                power_output_kw
            )

            message = {
                "propulsion_id": self.propulsion_id,
                "timestamp": time.time(),
                "load_ratio": float(load_ratio),
                "power_output_kw": float(power_output_kw),
                "power_input_kw": float(power_input_kw),
                "allocated_power_kw": float(allocated_power_kw),
                "speed_rpm": self._speed_rpm(float(load_ratio)),
            }

            with self._lock:
                # Keep ramping the internal setpoint regardless of the cap, so the
                # drive resumes ramping toward target once more power is available.
                self._ramped_target_load_ratio = current
                # But report the power-limited outcome as the actual current load.
                self._achieved_load_ratio = float(load_ratio)
                self._last_message = message

            self._send(KAFKA_TOPIC, key=self.propulsion_id, value=message)
            print(
                f"load={message['load_ratio'] * 100:.1f}% "
                f"power_output={message['power_output_kw']:.1f}kW "
                f"power_input={message['power_input_kw']:.1f}kW "
                f"allocated={message['allocated_power_kw']:.1f}kW "
                f"speed_rpm={message['speed_rpm']:.1f}rpm"
            )

            self._stop_event.wait(STEP_INTERVAL_S)
