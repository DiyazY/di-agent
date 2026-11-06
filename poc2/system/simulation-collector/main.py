"""Read-only Kafka snapshot adapter for di-agent HTTP collector protocol v1."""

import json
import logging
import math
import os
import re
import threading
import time
from dataclasses import dataclass
from typing import Any

from fastapi import FastAPI, HTTPException, Response
from kafka import KafkaConsumer
from kafka.errors import KafkaError

from schema import DEFAULT_INCLUDE, FIELDS, selected_metrics

logging.basicConfig(level=os.environ.get("LOG_LEVEL", "INFO"))
logger = logging.getLogger("simulation-collector")

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "kafka:9092").split(",")
KAFKA_GROUP_ID = os.environ.get("KAFKA_GROUP_ID", "simulation-collector")
TOPICS = {
    os.environ.get(f"{kind.upper()}_KAFKA_TOPIC", f"{kind}.telemetry"): kind
    for kind in FIELDS
    if kind != "allocation"
}
MAX_ACTIVE_SERIES = int(os.environ.get("MAX_ACTIVE_SERIES", "2000"))
MAX_AGE_SECONDS = float(os.environ.get("MAX_AGE_SECONDS", "15"))
SUBJECT_RE = re.compile(r"^[A-Za-z0-9._:-]+$")
SEGMENT_RE = re.compile(r"^[A-Za-z0-9._-]+$")
if MAX_ACTIVE_SERIES < 1 or not math.isfinite(MAX_AGE_SECONDS) or MAX_AGE_SECONDS <= 0:
    raise ValueError("MAX_ACTIVE_SERIES and MAX_AGE_SECONDS must be positive")
if len(TOPICS) != 6:
    raise ValueError("simulation telemetry topics must be distinct")


def subject_segment(identifier: str) -> str:
    # Reserve hex- so encoded human-readable IDs cannot collide with literal IDs.
    if SEGMENT_RE.fullmatch(identifier) and not identifier.startswith("hex-"):
        return identifier
    return "hex-" + identifier.encode("utf-8").hex()


def finite_number(value: Any) -> bool:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return False
    try:
        return math.isfinite(float(value))
    except OverflowError:
        return False


@dataclass
class Snapshot:
    timestamp: float
    samples: list[dict[str, Any]]
    emitted_second: int = 0
    pending: bool = True


class SimulationCollector:
    def __init__(
        self, *, max_active_series: int = MAX_ACTIVE_SERIES,
        max_age_seconds: float = MAX_AGE_SECONDS,
    ) -> None:
        self._lock = threading.Lock()
        self._snapshots: dict[str, Snapshot] = {}
        self._include = DEFAULT_INCLUDE
        self._available = selected_metrics(self._include)
        self._owner: tuple[str, str] | None = None
        self._max_series = max_active_series
        self._max_age = max_age_seconds
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self._connected = False
        self._last_error: str | None = None
        self._received = 0
        self._dropped = 0
        self._invalid_fields = 0
        self._superseded = 0
        self._expired = 0

    def start(self) -> None:
        self._thread = threading.Thread(
            target=self._consume, name="simulation-kafka-consumer", daemon=True,
        )
        self._thread.start()

    def stop(self) -> None:
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=10)

    def _new_consumer(self) -> KafkaConsumer:
        return KafkaConsumer(
            *TOPICS, bootstrap_servers=KAFKA_BROKERS,
            group_id=KAFKA_GROUP_ID, enable_auto_commit=True,
            auto_offset_reset="latest",
        )

    def _consume(self) -> None:
        try:
            while not self._stop.is_set():
                consumer = None
                try:
                    consumer = self._new_consumer()
                    while not self._stop.is_set():
                        records = consumer.poll(timeout_ms=1000)
                        with self._lock:
                            self._connected = bool(consumer.assignment())
                            self._last_error = None
                        for batch in records.values():
                            for message in batch:
                                self.ingest_json(message.topic, message.value)
                except (KafkaError, OSError) as exc:
                    with self._lock:
                        self._connected = False
                        self._last_error = str(exc)
                    logger.exception("Kafka consumer failed; reconnecting in 5 seconds")
                    self._stop.wait(5)
                finally:
                    with self._lock:
                        self._connected = False
                    if consumer is not None:
                        consumer.close()
        except Exception as exc:
            with self._lock:
                self._last_error = str(exc)
            logger.exception("Simulation consumer worker stopped unexpectedly")

    def _prune(self, now: float) -> None:
        for subject in list(self._snapshots):
            if now - self._snapshots[subject].timestamp > self._max_age:
                snapshot = self._snapshots.pop(subject)
                if snapshot.pending:
                    self._expired += 1

    def _drop(self, reason: str) -> bool:
        with self._lock:
            self._dropped += 1
        logger.warning("Dropping simulation message: %s", reason)
        return False

    def ingest_json(self, topic: str, raw: str | bytes, *, now: float | None = None) -> bool:
        now = time.time() if now is None else now
        try:
            kind = TOPICS[topic]
            message = json.loads(raw)
            if not isinstance(message, dict):
                raise ValueError("message must be an object")
            payload = message.get("payload", message)
            if not isinstance(payload, dict):
                raise ValueError("payload must be an object")
            payload = dict(payload)
            for key in ("source_id", "source_type", "timestamp", "schema_version"):
                if key in message:
                    payload.setdefault(key, message[key])
            version = payload.get("schema_version", 1)
            if isinstance(version, bool) or version != 1:
                raise ValueError("only simulation schema version 1 is supported")
            if payload.get("source_type", kind) != kind:
                raise ValueError("source_type does not match its telemetry topic")
            source_id = payload.get(f"{kind}_id", payload.get("source_id"))
            if not isinstance(source_id, str) or not source_id:
                raise ValueError(f"{kind}_id or source_id is required")
            if "source_id" in payload and source_id != payload["source_id"]:
                raise ValueError("source_id conflicts with the device id")
            timestamp = payload["timestamp"]
            if isinstance(timestamp, bool) or not isinstance(timestamp, (int, float)):
                raise ValueError("timestamp must be numeric Unix seconds")
            if not finite_number(timestamp) or timestamp <= 0:
                raise ValueError("timestamp must be finite and positive")
            if now - timestamp > self._max_age or timestamp > now + 2:
                raise ValueError("timestamp is stale or in the future")
            subjects = [(kind, f"{kind}:{subject_segment(source_id)}")]
            if kind == "switchboard":
                consumer_id = payload.get("consumer_id")
                if not isinstance(consumer_id, str) or not consumer_id:
                    raise ValueError("switchboard message requires consumer_id")
                subjects.append((
                    "allocation",
                    f"allocation:{subject_segment(source_id)}:{subject_segment(consumer_id)}",
                ))
            for _, subject in subjects:
                if len(subject) > 200 or not SUBJECT_RE.fullmatch(subject):
                    raise ValueError("device id cannot be represented as a subject")
        except (KeyError, TypeError, ValueError, UnicodeDecodeError) as exc:
            return self._drop(str(exc))

        with self._lock:
            self._prune(now)
            updates: dict[str, Snapshot] = {}
            invalid_fields = []
            for metric_kind, subject in subjects:
                previous = self._snapshots.get(subject)
                if previous and (
                    timestamp <= previous.timestamp
                    or int(timestamp) <= previous.emitted_second
                ):
                    self._superseded += 1
                    continue
                samples = []
                for field, declaration in FIELDS[metric_kind].items():
                    metric_type = f"{metric_kind}.{field}"
                    if field not in payload or metric_type not in self._available:
                        continue
                    value = payload[field]
                    if declaration.boolean:
                        valid = isinstance(value, bool)
                    else:
                        valid = finite_number(value)
                    if not valid:
                        invalid_fields.append(metric_type)
                        continue
                    sample = {
                        "metric_type": metric_type, "value": float(value),
                        "timestamp_unix": int(timestamp), "subject": subject,
                        "unit": declaration.unit, "chart": metric_kind,
                        "labels": {"origin": "simulation", "source_id": source_id},
                    }
                    if metric_kind == "battery" and "cycle_type" in payload:
                        cycle = payload["cycle_type"]
                        if cycle in ("charge", "discharge", "idle"):
                            sample["labels"]["cycle_type"] = cycle
                        else:
                            invalid_fields.append("battery.cycle_type")
                    if declaration.value_range is not None:
                        sample["range"] = list(declaration.value_range)
                    samples.append(sample)
                if samples:
                    updates[subject] = Snapshot(
                        timestamp, samples,
                        emitted_second=previous.emitted_second if previous else 0,
                    )
            series = sum(
                len(snapshot.samples) for subject, snapshot in self._snapshots.items()
                if subject not in updates
            ) + sum(len(snapshot.samples) for snapshot in updates.values())
            # Bound both metric storage and retained timestamp watermarks.
            subjects_count = len(self._snapshots.keys() | updates.keys())
            if series > self._max_series or subjects_count > self._max_series:
                self._dropped += 1
                logger.warning("Dropping simulation message: active series limit reached")
                return False
            self._invalid_fields += len(invalid_fields)
            self._snapshots.update(updates)
            self._received += 1
        if invalid_fields:
            logger.warning("Skipping invalid simulation fields: %s", ", ".join(invalid_fields))
        return True

    def hello(self, request: dict[str, Any]) -> dict[str, int]:
        if request.get("protocol") != 1 or isinstance(request.get("protocol"), bool):
            raise HTTPException(400, "only collector protocol 1 is supported")
        name, node = request.get("name"), request.get("node_id")
        if not isinstance(name, str) or not name or not isinstance(node, str) or not node:
            raise HTTPException(400, "hello requires name and node_id")
        config = request.get("config", {})
        if not isinstance(config, dict) or set(config) - {"include"}:
            raise HTTPException(400, "config supports only an include list")
        include = config.get("include", list(DEFAULT_INCLUDE))
        if (
            not isinstance(include, list) or not include
            or not all(isinstance(pattern, str) and pattern for pattern in include)
        ):
            raise HTTPException(400, "include must be a non-empty list of metric patterns")
        patterns = tuple(include)
        available = selected_metrics(patterns)
        if not available:
            raise HTTPException(400, "include does not select any supported simulation metrics")
        with self._lock:
            if self._owner is not None and self._owner != (name, node):
                raise HTTPException(409, "shared collector already belongs to another agent")
            if patterns != self._include:
                self._snapshots.clear()
            self._include = patterns
            self._available = available
            self._owner = (name, node)
        return {"protocol": 1}

    def collect(self, request: dict[str, Any]) -> dict[str, Any]:
        seq, now = request.get("seq"), request.get("now_unix")
        if isinstance(seq, bool) or not isinstance(seq, int) or seq < 0:
            raise HTTPException(400, "collect requires a non-negative seq")
        if isinstance(now, bool) or not isinstance(now, int) or now <= 0:
            raise HTTPException(400, "collect requires positive now_unix Unix seconds")
        with self._lock:
            if self._owner is None:
                raise HTTPException(409, "hello must be received before collect")
            # Use wall time: protocol now_unix may lead the clock in compliance checks.
            self._prune(time.time())
            samples = []
            for subject in sorted(self._snapshots):
                snapshot = self._snapshots[subject]
                if snapshot.pending:
                    samples.extend(snapshot.samples)
                    snapshot.pending = False
                    snapshot.emitted_second = int(snapshot.timestamp)
            return {"seq": seq, "available": list(self._available), "samples": samples}

    def status(self) -> dict[str, Any]:
        with self._lock:
            self._prune(time.time())
            return {
                "connected": self._connected, "last_error": self._last_error,
                "available_metrics": list(self._available),
                "pending_series": sum(
                    len(snapshot.samples) for snapshot in self._snapshots.values()
                    if snapshot.pending
                ),
                "received": self._received, "dropped": self._dropped,
                "invalid_fields": self._invalid_fields, "superseded": self._superseded,
                "expired": self._expired,
            }

    def healthy(self) -> bool:
        return self._thread is not None and self._thread.is_alive()


collector = SimulationCollector()
app = FastAPI(title="di-agent simulation collector")


@app.on_event("startup")
def start_consumer() -> None:
    collector.start()


@app.on_event("shutdown")
def stop_consumer() -> None:
    collector.stop()


@app.post("/collect")
def collect(request: dict[str, Any]) -> dict[str, Any]:
    if request.get("op") == "hello":
        return collector.hello(request)
    if request.get("op") == "collect":
        return collector.collect(request)
    raise HTTPException(400, "op must be hello or collect")


@app.get("/health")
def health(response: Response) -> dict[str, str]:
    healthy = collector.healthy()
    response.status_code = 200 if healthy else 503
    return {"status": "ok" if healthy else "unavailable"}


@app.get("/ready")
def ready(response: Response) -> dict[str, Any]:
    status = collector.status()
    response.status_code = 200 if status["connected"] else 503
    return status
