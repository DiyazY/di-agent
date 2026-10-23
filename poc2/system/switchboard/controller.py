import json
import logging
import os
import threading
import time

from kafka import KafkaConsumer, KafkaProducer
from kafka.errors import KafkaTimeoutError

from modbus_client import ModbusPollingClient
from switchboard import ConsumerRequest, allocate_power, summarize_source_health

logger = logging.getLogger(__name__)

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "localhost:9092").split(",")
# Topic the switchboard publishes per-consumer allocations to.
KAFKA_TOPIC = os.environ.get("KAFKA_TOPIC", "switchboard.telemetry")
# Consumers (propulsion, hotel load, ...) publish their power demand here.
REQUEST_KAFKA_TOPIC = os.environ.get("REQUEST_KAFKA_TOPIC", "switchboard.requests")
SWITCHBOARD_ID = os.environ.get("SWITCHBOARD_ID", "switchboard-1")
STEP_INTERVAL_S = float(os.environ.get("STEP_INTERVAL_S", "1"))
# A consumer request or genset/battery Modbus reading is dropped from the
# allocation if it's not fresh within this many seconds (treated as offline).
STALE_TIMEOUT_S = float(os.environ.get("STALE_TIMEOUT_S", "5"))
# "id@host:port" comma lists of genset/battery Modbus TCP servers to poll.
# This is the switchboard's actual source of genset/battery status/supply,
# superseding what used to be a Kafka consumer of genset.telemetry/
# battery.telemetry (genset/battery still publish those topics for the
# telemetry-writer/Grafana pipeline, which is unrelated to this).
GENSET_MODBUS_TARGETS = os.environ.get("GENSET_MODBUS_TARGETS", "")
BATTERY_MODBUS_TARGETS = os.environ.get("BATTERY_MODBUS_TARGETS", "")
MODBUS_POLL_INTERVAL_S = float(os.environ.get("MODBUS_POLL_INTERVAL_S", "2"))


def _deserialize_value(raw_value: bytes) -> dict | None:
    try:
        value = json.loads(raw_value.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError, TypeError):
        logger.warning("Ignoring malformed Kafka payload")
        return None
    if not isinstance(value, dict):
        logger.warning("Ignoring Kafka payload that is not an object")
        return None
    return value


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
                value_deserializer=_deserialize_value,
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
        # consumer_id -> (requested_power_kw, priority, received_at)
        self._consumer_requests: dict[str, tuple[float, int, float]] = {}
        self._last_allocations: dict[str, float] = {}

        self._producer: KafkaProducer | None = None
        self._request_consumer: KafkaConsumer | None = None
        self._stop_event = threading.Event()
        self._request_thread: threading.Thread | None = None
        self._allocation_thread: threading.Thread | None = None
        self._errors: list[str] = []
        self._modbus_client = ModbusPollingClient(
            genset_targets=GENSET_MODBUS_TARGETS,
            battery_targets=BATTERY_MODBUS_TARGETS,
            poll_interval_s=MODBUS_POLL_INTERVAL_S,
        )

    def start(self) -> None:
        self._producer = _make_producer()
        self._request_consumer = _make_consumer(REQUEST_KAFKA_TOPIC)
        self._request_thread = threading.Thread(target=self._consume_requests, daemon=True)
        self._request_thread.start()
        self._allocation_thread = threading.Thread(target=self._run, daemon=True)
        self._allocation_thread.start()
        self._modbus_client.start()

    def stop(self) -> None:
        self._stop_event.set()
        self._modbus_client.stop()
        if self._request_consumer is not None:
            self._request_consumer.close()
        for thread in (self._request_thread, self._allocation_thread):
            if thread is not None:
                thread.join(timeout=STEP_INTERVAL_S * 2)
        if self._producer is not None:
            self._producer.flush()
            self._producer.close()

    def get_status(self) -> dict:
        gensets, batteries = self._get_modbus_sources()
        with self._lock:
            consumers = {
                consumer_id: {
                    "requested_power_kw": requested_power_kw,
                    "priority": priority,
                    "allocated_power_kw": self._last_allocations.get(consumer_id, 0.0),
                    "stale": self._is_stale(received_at),
                }
                for consumer_id, (requested_power_kw, priority, received_at)
                in self._consumer_requests.items()
            }
            return {
                "switchboard_id": self.switchboard_id,
                "available_supply_kw": self._get_available_supply_kw(gensets, batteries),
                "total_demand_kw": self._get_total_demand_kw(),
                "total_co2_kg_per_s": self._get_total_co2_kg_per_s(gensets),
                "total_nox_kg_per_s": self._get_total_nox_kg_per_s(gensets),
                "gensets": gensets,
                "batteries": batteries,
                "consumers": consumers,
                "source_health": summarize_source_health(gensets, batteries, consumers),
                "data_quality": {
                    "total_sources": len(gensets) + len(batteries),
                    "stale_sources": sum(1 for item in gensets.values() if item["stale"]) + sum(1 for item in batteries.values() if item["stale"]),
                    "total_consumers": len(consumers),
                    "stale_consumers": sum(1 for item in consumers.values() if item["stale"]),
                },
            }

    def get_health(self) -> dict:
        threads = {
            "request_consumer": self._request_thread,
            "allocation": self._allocation_thread,
        }
        with self._lock:
            errors = list(self._errors)
        thread_status = {
            name: thread is not None and thread.is_alive() for name, thread in threads.items()
        }
        healthy = all(thread_status.values()) and not errors
        return {"status": "ok" if healthy else "error", "threads": thread_status, "errors": errors}

    def get_modbus_status(self) -> dict:
        """Raw per-target view of the same Modbus polling this controller
        uses internally for genset/battery status and allocation."""
        return self._modbus_client.get_status()

    def _record_error(self, message: str) -> None:
        with self._lock:
            self._errors.append(message)

    def _send(self, topic: str, *, key: str, value: dict) -> None:
        self._producer.send(topic, key=key, value=value).get(timeout=5)

    def _is_stale(self, received_at: float) -> bool:
        return time.time() - received_at > STALE_TIMEOUT_S

    def _get_modbus_sources(self) -> tuple[dict, dict]:
        """Splits the Modbus client's raw per-target readings into the
        genset/battery status views used for both get_status() and the
        allocation loop. A target is stale if the poller flagged it stale
        (connection/read failure) or its last successful read is too old."""
        gensets: dict[str, dict] = {}
        batteries: dict[str, dict] = {}
        for target_id, reading in self._modbus_client.get_status().items():
            fetched_at = reading.get("fetched_at")
            stale = reading.get("stale", True) or fetched_at is None or self._is_stale(fetched_at)
            if reading.get("source_type") == "genset":
                gensets[target_id] = {
                    "power_kw": reading.get("power_kw", 0.0),
                    "co2_kg_per_s": reading.get("co2_kg_per_s", 0.0),
                    "nox_kg_per_s": reading.get("nox_kg_per_s", 0.0),
                    "stale": stale,
                }
            elif reading.get("source_type") == "battery":
                batteries[target_id] = {
                    "power_kw": reading.get("power_kw", 0.0),
                    "stale": stale,
                }
        return gensets, batteries

    def _consume_requests(self) -> None:
        try:
            for record in self._request_consumer:
                if self._stop_event.is_set():
                    break
                try:
                    value = record.value
                    if value is None:
                        continue
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
                except Exception as error:
                    logger.warning("Ignoring malformed consumer request: %s", error)
        except Exception:
            if not self._stop_event.is_set():
                logger.exception("Request consumer stopped unexpectedly")
                self._record_error("request_consumer stopped unexpectedly")

    def _get_available_supply_kw(self, gensets: dict, batteries: dict) -> float:
        """Sum of power output from gensets and batteries whose Modbus
        reading isn't stale."""
        return sum(g["power_kw"] for g in gensets.values() if not g["stale"]) + sum(
            b["power_kw"] for b in batteries.values() if not b["stale"]
        )

    def _get_total_co2_kg_per_s(self, gensets: dict) -> float:
        """Whole-system CO2 emission rate: sum across all non-stale gensets."""
        return sum(g["co2_kg_per_s"] for g in gensets.values() if not g["stale"])

    def _get_total_nox_kg_per_s(self, gensets: dict) -> float:
        """Whole-system NOx emission rate: sum across all non-stale gensets."""
        return sum(g["nox_kg_per_s"] for g in gensets.values() if not g["stale"])

    def _get_total_demand_kw(self) -> float:
        """Must be called with self._lock held."""
        return sum(
            requested_power_kw
            for requested_power_kw, _priority, received_at in self._consumer_requests.values()
            if not self._is_stale(received_at)
        )

    def _run(self) -> None:
        try:
            self._run_loop()
        except Exception:
            logger.exception("Allocation worker stopped unexpectedly")
            self._record_error("allocation worker stopped unexpectedly")

    def _run_loop(self) -> None:
        while not self._stop_event.is_set():
            gensets, batteries = self._get_modbus_sources()
            with self._lock:
                available_supply_kw = self._get_available_supply_kw(gensets, batteries)
                total_co2_kg_per_s = self._get_total_co2_kg_per_s(gensets)
                total_nox_kg_per_s = self._get_total_nox_kg_per_s(gensets)
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
                payload = {
                    "switchboard_id": self.switchboard_id,
                    "consumer_id": request.consumer_id,
                    "timestamp": timestamp,
                    "requested_power_kw": request.requested_power_kw,
                    "allocated_power_kw": allocations.get(request.consumer_id, 0.0),
                    "available_supply_kw": available_supply_kw,
                    "total_demand_kw": total_demand_kw,
                    "total_co2_kg_per_s": total_co2_kg_per_s,
                    "total_nox_kg_per_s": total_nox_kg_per_s,
                }
                message = {
                    "event": "switchboard.telemetry",
                    "schema_version": 1,
                    "source_id": self.switchboard_id,
                    "source_type": "switchboard",
                    "timestamp": timestamp,
                    "payload": payload,
                }
                message.update(payload)
                self._send(KAFKA_TOPIC, key=request.consumer_id, value=message)

            allocated_str = ", ".join(
                f"{cid}={power_kw:.1f}kW" for cid, power_kw in allocations.items()
            )
            print(
                f"supply={available_supply_kw:.1f}kW demand={total_demand_kw:.1f}kW "
                f"co2={total_co2_kg_per_s:.5f}kg/s nox={total_nox_kg_per_s:.6f}kg/s "
                f"allocations=[{allocated_str}]"
            )

            self._stop_event.wait(STEP_INTERVAL_S)
