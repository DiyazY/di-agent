import json
import os
import threading
import time

from kafka import KafkaConsumer, KafkaProducer
from kafka.errors import KafkaTimeoutError

from switchboard import ConsumerRequest, allocate_power

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "localhost:9092").split(",")
# Topic the switchboard publishes per-consumer allocations to.
KAFKA_TOPIC = os.environ.get("KAFKA_TOPIC", "switchboard.telemetry")
# Genset telemetry: sums into the power available on the bus.
GENSET_KAFKA_TOPIC = os.environ.get("GENSET_KAFKA_TOPIC", "genset.telemetry")
# Consumers (propulsion, hotel load, ...) publish their power demand here.
REQUEST_KAFKA_TOPIC = os.environ.get("REQUEST_KAFKA_TOPIC", "switchboard.requests")
SWITCHBOARD_ID = os.environ.get("SWITCHBOARD_ID", "switchboard-1")
STEP_INTERVAL_S = float(os.environ.get("STEP_INTERVAL_S", "1"))
# A genset or consumer is dropped from the allocation if no message was seen
# within this many seconds (treated as offline).
STALE_TIMEOUT_S = float(os.environ.get("STALE_TIMEOUT_S", "5"))


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


def _make_consumer(*topics: str) -> KafkaConsumer:
    while True:
        try:
            return KafkaConsumer(
                *topics,
                bootstrap_servers=KAFKA_BROKERS,
                value_deserializer=lambda v: json.loads(v.decode("utf-8")),
                auto_offset_reset="latest",
                group_id=None,
            )
        except KafkaTimeoutError:
            print(f"Kafka brokers {KAFKA_BROKERS} not available yet, retrying in 5s ...")
            time.sleep(5)


class SwitchboardController:
    """Central power-management authority sitting between any number of
    gensets and any number of power consumers.

    Instead of every consumer summing genset telemetry itself (which doesn't
    scale past one genset/one consumer and has no notion of priority), the
    switchboard is the single place that tracks total available supply and
    total requested demand, decides how to split the available power across
    consumers, and publishes each consumer's grant. Consumers only ever look
    at their own allocation."""

    def __init__(self) -> None:
        self.switchboard_id = SWITCHBOARD_ID

        self._lock = threading.Lock()
        # genset_id -> (power_kw, received_at)
        self._genset_power_kw: dict[str, tuple[float, float]] = {}
        # consumer_id -> (requested_power_kw, priority, received_at)
        self._consumer_requests: dict[str, tuple[float, int, float]] = {}
        self._last_allocations: dict[str, float] = {}

        self._producer: KafkaProducer | None = None
        self._genset_consumer: KafkaConsumer | None = None
        self._request_consumer: KafkaConsumer | None = None
        self._stop_event = threading.Event()
        self._genset_thread: threading.Thread | None = None
        self._request_thread: threading.Thread | None = None
        self._allocation_thread: threading.Thread | None = None

    def start(self) -> None:
        self._producer = _make_producer()
        self._genset_consumer = _make_consumer(GENSET_KAFKA_TOPIC)
        self._request_consumer = _make_consumer(REQUEST_KAFKA_TOPIC)
        self._genset_thread = threading.Thread(target=self._consume_genset_telemetry, daemon=True)
        self._genset_thread.start()
        self._request_thread = threading.Thread(target=self._consume_requests, daemon=True)
        self._request_thread.start()
        self._allocation_thread = threading.Thread(target=self._run, daemon=True)
        self._allocation_thread.start()

    def stop(self) -> None:
        self._stop_event.set()
        if self._genset_consumer is not None:
            self._genset_consumer.close()
        if self._request_consumer is not None:
            self._request_consumer.close()
        for thread in (self._genset_thread, self._request_thread, self._allocation_thread):
            if thread is not None:
                thread.join(timeout=STEP_INTERVAL_S * 2)
        if self._producer is not None:
            self._producer.flush()
            self._producer.close()

    def get_status(self) -> dict:
        with self._lock:
            return {
                "switchboard_id": self.switchboard_id,
                "available_supply_kw": self._get_available_supply_kw(),
                "total_demand_kw": self._get_total_demand_kw(),
                "gensets": {
                    genset_id: {
                        "power_kw": power_kw,
                        "stale": self._is_stale(received_at),
                    }
                    for genset_id, (power_kw, received_at) in self._genset_power_kw.items()
                },
                "consumers": {
                    consumer_id: {
                        "requested_power_kw": requested_power_kw,
                        "priority": priority,
                        "allocated_power_kw": self._last_allocations.get(consumer_id, 0.0),
                        "stale": self._is_stale(received_at),
                    }
                    for consumer_id, (requested_power_kw, priority, received_at)
                    in self._consumer_requests.items()
                },
            }

    def _is_stale(self, received_at: float) -> bool:
        return time.time() - received_at > STALE_TIMEOUT_S

    def _consume_genset_telemetry(self) -> None:
        try:
            for record in self._genset_consumer:
                if self._stop_event.is_set():
                    break
                value = record.value
                genset_id = value.get("genset_id", record.key)
                power_kw = value.get("power_kw")
                if power_kw is None:
                    continue
                with self._lock:
                    self._genset_power_kw[genset_id] = (float(power_kw), time.time())
        except Exception:
            if not self._stop_event.is_set():
                raise

    def _consume_requests(self) -> None:
        try:
            for record in self._request_consumer:
                if self._stop_event.is_set():
                    break
                value = record.value
                consumer_id = value.get("consumer_id", record.key)
                requested_power_kw = value.get("requested_power_kw")
                if consumer_id is None or requested_power_kw is None:
                    continue
                priority = int(value.get("priority", 1))
                with self._lock:
                    self._consumer_requests[consumer_id] = (
                        float(requested_power_kw),
                        priority,
                        time.time(),
                    )
        except Exception:
            if not self._stop_event.is_set():
                raise

    def _get_available_supply_kw(self) -> float:
        """Sum of power output from gensets whose telemetry isn't stale. Must
        be called with self._lock held."""
        return sum(
            power_kw
            for power_kw, received_at in self._genset_power_kw.values()
            if not self._is_stale(received_at)
        )

    def _get_total_demand_kw(self) -> float:
        """Must be called with self._lock held."""
        return sum(
            requested_power_kw
            for requested_power_kw, _priority, received_at in self._consumer_requests.values()
            if not self._is_stale(received_at)
        )

    def _run(self) -> None:
        while not self._stop_event.is_set():
            with self._lock:
                available_supply_kw = self._get_available_supply_kw()
                active_requests = [
                    ConsumerRequest(consumer_id, requested_power_kw, priority, received_at)
                    for consumer_id, (requested_power_kw, priority, received_at)
                    in self._consumer_requests.items()
                    if not self._is_stale(received_at)
                ]

            total_demand_kw = sum(request.requested_power_kw for request in active_requests)
            allocations = allocate_power(available_supply_kw, active_requests)
            timestamp = time.time()

            with self._lock:
                self._last_allocations = allocations

            for request in active_requests:
                message = {
                    "switchboard_id": self.switchboard_id,
                    "consumer_id": request.consumer_id,
                    "timestamp": timestamp,
                    "requested_power_kw": request.requested_power_kw,
                    "allocated_power_kw": allocations.get(request.consumer_id, 0.0),
                    "available_supply_kw": available_supply_kw,
                    "total_demand_kw": total_demand_kw,
                }
                self._producer.send(KAFKA_TOPIC, key=request.consumer_id, value=message)

            allocated_str = ", ".join(
                f"{cid}={power_kw:.1f}kW" for cid, power_kw in allocations.items()
            )
            print(
                f"supply={available_supply_kw:.1f}kW demand={total_demand_kw:.1f}kW "
                f"allocations=[{allocated_str}]"
            )

            self._stop_event.wait(STEP_INTERVAL_S)
