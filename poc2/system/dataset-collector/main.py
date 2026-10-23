"""HTTP collector plugin that turns dataset Kafka events into MetricSamples."""

import json
import logging
import math
import os
import re
import threading
from dataclasses import dataclass, field
from typing import Any

from fastapi import FastAPI, HTTPException, Response
from kafka import KafkaConsumer

logging.basicConfig(level=os.environ.get("LOG_LEVEL", "INFO"))
logger = logging.getLogger("dataset-collector")

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "kafka:9092").split(",")
KAFKA_TOPIC = os.environ.get("KAFKA_TOPIC", "dataset.telemetry")
KAFKA_GROUP_ID = os.environ.get("KAFKA_GROUP_ID", "dataset-collector")
MAX_ACTIVE_SERIES = int(os.environ.get("MAX_ACTIVE_SERIES", "2000"))
MAX_LABEL_SOURCES = 4
METRIC_RE = re.compile(r"^[A-Za-z0-9._-]+$")
SUBJECT_RE = re.compile(r"^[A-Za-z0-9._:-]+$")
if MAX_ACTIVE_SERIES < 1:
    raise ValueError("MAX_ACTIVE_SERIES must be positive")


@dataclass
class Aggregate:
    count: int = 0
    mean: float = 0.0
    sources: set[str] = field(default_factory=set)
    origins: set[str] = field(default_factory=set)
    multiple_sources: bool = False

    def add(self, value: float, source: str, origin: str) -> None:
        self.count += 1
        self.mean += (value - self.mean) / self.count
        if source not in self.sources and len(self.sources) >= MAX_LABEL_SOURCES:
            self.multiple_sources = True
        else:
            self.sources.add(source)
        self.origins.add(origin)


class DatasetCollector:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._pending: dict[tuple[str, str], Aggregate] = {}
        self._available: set[str] = set()
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self._connected = False
        self._last_error: str | None = None
        self._received = 0
        self._dropped = 0
        self._hello: dict[str, Any] | None = None

    def start(self) -> None:
        self._thread = threading.Thread(
            target=self._consume, name="dataset-kafka-consumer", daemon=True
        )
        self._thread.start()

    def stop(self) -> None:
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=10)

    def _new_consumer(self) -> KafkaConsumer:
        return KafkaConsumer(
            KAFKA_TOPIC,
            bootstrap_servers=KAFKA_BROKERS,
            group_id=KAFKA_GROUP_ID,
            enable_auto_commit=True,
            auto_offset_reset="latest",
            value_deserializer=lambda value: value.decode("utf-8"),
        )

    def _consume(self) -> None:
        while not self._stop.is_set():
            consumer = None
            try:
                consumer = self._new_consumer()
                with self._lock:
                    self._connected = True
                    self._last_error = None
                while not self._stop.is_set():
                    for records in consumer.poll(timeout_ms=1000).values():
                        for message in records:
                            self.ingest_json(message.value)
            except Exception as exc:
                with self._lock:
                    self._connected = False
                    self._last_error = str(exc)
                logger.exception("Kafka consumer failed; reconnecting in 5 seconds")
                self._stop.wait(5)
            finally:
                if consumer is not None:
                    consumer.close()
                with self._lock:
                    self._connected = False

    def ingest_json(self, raw: str) -> bool:
        """Validate and fold one Kafka event into a bounded per-signal mean."""
        try:
            message = json.loads(raw)
            run_id = message["run_id"]
            signal = message["signal"]
            source = message["source"]
            origin = message["origin"]
            value = float(message["value"])
            if not isinstance(run_id, str) or not run_id or len(run_id) > 200:
                raise ValueError("run_id must be a non-empty string")
            if not isinstance(signal, str) or len(signal) > 200 or not METRIC_RE.fullmatch(signal):
                raise ValueError("signal is not a valid metric_type")
            if not isinstance(source, str) or not source or len(source) > 100:
                raise ValueError("source must be a non-empty string")
            if origin not in ("real", "synthetic"):
                raise ValueError("origin must be real or synthetic")
            if not math.isfinite(value):
                raise ValueError("value must be finite")
            if not SUBJECT_RE.fullmatch(f"engine:{run_id}"):
                raise ValueError("run_id cannot be represented in an engine subject")
        except (KeyError, TypeError, ValueError, json.JSONDecodeError) as exc:
            with self._lock:
                self._dropped += 1
            logger.warning("Dropping invalid dataset message: %s", exc)
            return False

        key = (run_id, signal)
        with self._lock:
            aggregate = self._pending.get(key)
            if aggregate is None:
                if (
                    len(self._pending) >= MAX_ACTIVE_SERIES
                    or (
                        signal not in self._available
                        and len(self._available) >= MAX_ACTIVE_SERIES
                    )
                ):
                    self._dropped += 1
                    logger.warning("Dropping dataset message: active series limit reached")
                    return False
                aggregate = Aggregate()
                self._pending[key] = aggregate
            self._available.add(signal)
            aggregate.add(value, source, origin)
            self._received += 1
        return True

    def hello(self, request: dict[str, Any]) -> dict[str, int]:
        if request.get("protocol") != 1:
            raise HTTPException(status_code=400, detail="only collector protocol 1 is supported")
        if not request.get("name") or not request.get("node_id"):
            raise HTTPException(status_code=400, detail="hello requires name and node_id")
        self._hello = request
        return {"protocol": 1}

    def collect(self, request: dict[str, Any]) -> dict[str, Any]:
        if self._hello is None:
            raise HTTPException(status_code=409, detail="hello must be received before collect")
        seq = request.get("seq")
        now_unix = request.get("now_unix")
        if not isinstance(seq, int) or isinstance(seq, bool) or seq < 0:
            raise HTTPException(status_code=400, detail="collect requires a non-negative seq")
        if not isinstance(now_unix, int) or isinstance(now_unix, bool) or now_unix <= 0:
            raise HTTPException(status_code=400, detail="collect requires a positive now_unix")

        with self._lock:
            pending = self._pending
            self._pending = {}

        samples = []
        for (run_id, signal), aggregate in sorted(pending.items()):
            sources = sorted(aggregate.sources)
            origins = sorted(aggregate.origins)
            labels = {
                "dataset_source": "multiple" if aggregate.multiple_sources else ",".join(sources),
                "origin": origins[0] if len(origins) == 1 else "mixed",
            }
            samples.append({
                "metric_type": signal,
                "value": aggregate.mean,
                "timestamp_unix": now_unix,
                "subject": f"engine:{run_id}",
                "unit": "unknown",
                "labels": labels,
            })
        return {"seq": seq, "samples": samples}

    def status(self) -> dict[str, Any]:
        with self._lock:
            return {
                "connected": self._connected,
                "available_metrics": sorted(self._available),
                "pending_series": len(self._pending),
                "received": self._received,
                "dropped": self._dropped,
                "last_error": self._last_error,
            }

    def healthy(self) -> bool:
        return self._thread is not None and self._thread.is_alive()


collector = DatasetCollector()
app = FastAPI(title="di-agent dataset collector")


@app.on_event("startup")
def start_consumer() -> None:
    collector.start()


@app.on_event("shutdown")
def stop_consumer() -> None:
    collector.stop()


@app.post("/collect")
def collect(request: dict[str, Any]) -> dict[str, Any]:
    operation = request.get("op")
    if operation == "hello":
        return collector.hello(request)
    if operation == "collect":
        return collector.collect(request)
    raise HTTPException(status_code=400, detail="op must be hello or collect")


@app.get("/health")
def health(response: Response) -> dict[str, str]:
    response.status_code = 200 if collector.healthy() else 503
    return {"status": "ok" if collector.healthy() else "unavailable"}


@app.get("/ready")
def ready(response: Response) -> dict[str, Any]:
    status = collector.status()
    response.status_code = 200 if status["connected"] else 503
    return status
