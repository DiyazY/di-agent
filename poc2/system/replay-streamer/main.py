"""Replay real engine data or generate profile-matched synthetic telemetry."""

import contextlib
import heapq
import itertools
import json
import logging
import math
import os
import random
import threading
import time
from dataclasses import asdict, dataclass
from typing import Iterator, Literal

import psycopg
from fastapi import FastAPI, HTTPException, Response
from kafka import KafkaProducer
from pydantic import BaseModel, Field

logging.basicConfig(level=os.environ.get("LOG_LEVEL", "INFO"))
logger = logging.getLogger("replay-streamer")

PG_CONFIG = {
    "host": os.environ.get("PGHOST", "postgres"),
    "port": int(os.environ.get("PGPORT", "5432")),
    "dbname": os.environ.get("PGDATABASE", "engine"),
    "user": os.environ.get("PGUSER", "di"),
    "password": os.environ.get("PGPASSWORD", ""),
}
KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "kafka:9092").split(",")
KAFKA_TOPIC = os.environ.get("KAFKA_TOPIC", "dataset.telemetry")
READ_AHEAD_ROWS = int(os.environ.get("READ_AHEAD_ROWS", "1000"))


@dataclass(frozen=True)
class StreamConfig:
    mode: Literal["real", "synthetic"]
    run_id: str
    interval_ms: int
    points_per_interval: int
    loop: bool
    random_seed: int


def _initial_config() -> StreamConfig:
    mode_value = os.environ.get("MODE", "synthetic")
    if mode_value == "real":
        mode: Literal["real", "synthetic"] = "real"
    elif mode_value == "synthetic":
        mode = "synthetic"
    else:
        raise ValueError("MODE must be either 'real' or 'synthetic'")
    interval_ms = int(os.environ.get("INTERVAL_MS", "1000"))
    points_per_interval = int(os.environ.get("POINTS_PER_INTERVAL", "1"))
    if not 10 <= interval_ms <= 3_600_000:
        raise ValueError("INTERVAL_MS must be between 10 and 3600000")
    if not 1 <= points_per_interval <= 10_000:
        raise ValueError("POINTS_PER_INTERVAL must be between 1 and 10000")
    if READ_AHEAD_ROWS < 1:
        raise ValueError("READ_AHEAD_ROWS must be positive")
    return StreamConfig(
        mode=mode,
        run_id=os.environ.get("RUN_ID", "1000rpm_150kW_0001"),
        interval_ms=interval_ms,
        points_per_interval=points_per_interval,
        loop=os.environ.get("LOOP", "true").lower() == "true",
        random_seed=int(os.environ.get("RANDOM_SEED", "1")),
    )


class ConfigUpdate(BaseModel):
    mode: Literal["real", "synthetic"] | None = None
    run_id: str | None = Field(default=None, min_length=1, max_length=200)
    interval_ms: int | None = Field(default=None, ge=10, le=3_600_000)
    points_per_interval: int | None = Field(default=None, ge=1, le=10_000)
    loop: bool | None = None
    random_seed: int | None = None


@dataclass(frozen=True)
class SignalProfile:
    source: str
    signal: str
    minimum: float
    maximum: float
    mean: float
    stddev: float
    sample_count: int
    first_t: float
    last_t: float

    @property
    def step(self) -> float:
        return max((self.last_t - self.first_t) / max(self.sample_count - 1, 1), 1e-9)


def _metadata(connection, run_id: str) -> dict[str, str | None]:
    rows = connection.execute(
        "SELECT parameter, value FROM run_metadata WHERE run_id = %s ORDER BY parameter",
        (run_id,),
    ).fetchall()
    return {str(key): None if value is None else str(value) for key, value in rows}


def _series_rows(cursor, run_id: str, source: str, signal: str) -> Iterator[tuple]:
    cursor.execute(
        """
        SELECT source, signal, t, value
        FROM timeseries
        WHERE run_id = %s AND source = %s AND signal = %s AND value IS NOT NULL
        ORDER BY t
        """,
        (run_id, source, signal),
    )
    while rows := cursor.fetchmany(READ_AHEAD_ROWS):
        yield from rows


def real_records(config: StreamConfig) -> Iterator[dict]:
    """Yield every source/signal of the run merged by source time.

    Each (source, signal) series is read through its own bounded server-side
    cursor and the series are merged lazily, so memory stays constant. Series
    share the time grid, so shorter ones simply end before the longer ones.
    """
    while True:
        with psycopg.connect(**PG_CONFIG) as connection:
            metadata = _metadata(connection, config.run_id)
            series = connection.execute(
                """
                SELECT DISTINCT source, signal FROM timeseries
                WHERE run_id = %s AND value IS NOT NULL
                ORDER BY source, signal
                """,
                (config.run_id,),
            ).fetchall()
            if not series:
                raise ValueError(f"no usable samples found for run_id={config.run_id!r}")
            with contextlib.ExitStack() as stack:
                streams = [
                    _series_rows(
                        stack.enter_context(connection.cursor(name=f"replay_{index}")),
                        config.run_id,
                        source,
                        signal,
                    )
                    for index, (source, signal) in enumerate(series)
                ]
                for source, signal, t_source, value in heapq.merge(
                    *streams, key=lambda row: (row[2], row[0], row[1])
                ):
                    yield {
                        "run_id": config.run_id,
                        "source": source,
                        "signal": signal,
                        "t_source": float(t_source),
                        "value": float(value),
                        "origin": "real",
                        "metadata": metadata,
                    }
        if not config.loop:
            return
        time.sleep(0.1)


def time_steps(records: Iterator[dict]) -> Iterator[list[dict]]:
    """Group the merged stream into steps: all samples sharing one source time."""
    for _, group in itertools.groupby(records, key=lambda record: record["t_source"]):
        yield list(group)


def load_profiles(config: StreamConfig) -> tuple[list[SignalProfile], dict[str, str | None]]:
    with psycopg.connect(**PG_CONFIG) as connection:
        rows = connection.execute(
            """
            SELECT source, signal, min(value), max(value), avg(value),
                   coalesce(stddev_samp(value), 0), count(*), min(t), max(t)
            FROM timeseries
            WHERE run_id = %s AND value IS NOT NULL
            GROUP BY source, signal
            ORDER BY source, signal
            """,
            (config.run_id,),
        ).fetchall()
        metadata_rows = connection.execute(
            "SELECT parameter, value FROM run_metadata WHERE run_id = %s ORDER BY parameter",
            (config.run_id,),
        ).fetchall()
    profiles = [
        SignalProfile(
            source=source,
            signal=signal,
            minimum=float(minimum),
            maximum=float(maximum),
            mean=float(mean),
            stddev=float(stddev),
            sample_count=int(count),
            first_t=float(first_t),
            last_t=float(last_t),
        )
        for source, signal, minimum, maximum, mean, stddev, count, first_t, last_t in rows
    ]
    if not profiles:
        raise ValueError(f"no usable samples found for run_id={config.run_id!r}")
    return profiles, {
        str(key): None if value is None else str(value)
        for key, value in metadata_rows
    }


def synthetic_records(config: StreamConfig) -> Iterator[dict]:
    """Generate bounded, correlated values using this run's actual profiles."""
    profiles, metadata = load_profiles(config)
    while True:
        rng = random.Random(config.random_seed)

        def signal_records(profile: SignalProfile) -> Iterator[dict]:
            previous = profile.mean
            for offset in range(profile.sample_count):
                # An AR(1) process retains each measured signal's spread and
                # yields values lazily so long recordings stay memory-bounded.
                value = profile.mean + 0.9 * (previous - profile.mean)
                value += rng.gauss(0.0, profile.stddev * math.sqrt(1 - 0.9**2))
                value = min(profile.maximum, max(profile.minimum, value))
                previous = value
                yield {
                    "run_id": config.run_id,
                    "source": profile.source,
                    "signal": profile.signal,
                    "t_source": round(profile.first_t + offset * profile.step, 6),
                    "value": value,
                    "origin": "synthetic",
                    "metadata": metadata,
                }

        streams = [signal_records(profile) for profile in profiles]
        yield from heapq.merge(
            *streams,
            key=lambda record: (record["t_source"], record["source"], record["signal"]),
        )
        if not config.loop:
            return


def _producer() -> KafkaProducer:
    while True:
        try:
            return KafkaProducer(
                bootstrap_servers=KAFKA_BROKERS,
                value_serializer=lambda value: json.dumps(value).encode("utf-8"),
                key_serializer=lambda key: key.encode("utf-8"),
            )
        except Exception:
            logger.exception("Kafka unavailable; retrying producer connection in 5 seconds")
            time.sleep(5)


class ReplayService:
    def __init__(self) -> None:
        self._lock = threading.RLock()
        self._config = _initial_config()
        self._revision = 0
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self._producer: KafkaProducer | None = None
        self._status: dict = {
            "state": "starting",
            "published": 0,
            "last_error": None,
        }

    def start(self) -> None:
        self._producer = _producer()
        self._thread = threading.Thread(target=self._run, name="replay-publisher", daemon=True)
        self._thread.start()

    def stop(self) -> None:
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=10)
        if self._producer:
            self._producer.close(timeout=5)

    def snapshot(self) -> tuple[StreamConfig, int]:
        with self._lock:
            return self._config, self._revision

    def update(self, update: ConfigUpdate) -> StreamConfig:
        with self._lock:
            self._config = StreamConfig(
                **(asdict(self._config) | update.model_dump(exclude_unset=True, exclude_none=True))
            )
            self._revision += 1
            return self._config

    def status(self) -> dict:
        with self._lock:
            return {"config": asdict(self._config), **self._status}

    def healthy(self) -> bool:
        return self._thread is not None and self._thread.is_alive()

    def _run(self) -> None:
        while not self._stop.is_set():
            config, revision = self.snapshot()
            self._set_status(state="running", last_error=None)
            try:
                records = real_records(config) if config.mode == "real" else synthetic_records(config)
                steps = time_steps(records)
                deadline = time.monotonic()
                while not self._stop.is_set():
                    current, current_revision = self.snapshot()
                    if current_revision != revision:
                        break
                    batch = []
                    for _ in range(config.points_per_interval):
                        try:
                            batch.extend(next(steps))
                        except StopIteration:
                            break
                    if not batch:
                        self._set_status(state="complete")
                        while not self._stop.wait(0.5):
                            _, updated_revision = self.snapshot()
                            if updated_revision != revision:
                                break
                        break
                    assert self._producer is not None
                    emitted_at = time.time()
                    for sample_index, record in enumerate(batch):
                        message = {
                            **record,
                            "timestamp": emitted_at,
                            "timestamp_ns": int(emitted_at * 1_000_000_000) + sample_index,
                        }
                        self._producer.send(
                            KAFKA_TOPIC,
                            key=f"{config.run_id}:{record['source']}:{record['signal']}",
                            value=message,
                        )
                    self._producer.flush(timeout=max(config.interval_ms / 1000, 1))
                    self._set_status(
                        state="running",
                        published=self._status["published"] + len(batch),
                        last_error=None,
                    )
                    deadline += config.interval_ms / 1000
                    delay = deadline - time.monotonic()
                    if delay <= 0:
                        deadline = time.monotonic()
                        continue
                    while delay > 0 and not self._stop.is_set():
                        if self._stop.wait(min(delay, 0.25)):
                            break
                        _, updated_revision = self.snapshot()
                        if updated_revision != revision:
                            break
                        delay = deadline - time.monotonic()
            except Exception as exc:
                logger.exception("Replay stream failed; retrying in 5 seconds")
                self._set_status(state="error", last_error=str(exc))
                self._stop.wait(5)

    def _set_status(self, **values: object) -> None:
        with self._lock:
            self._status.update(values)


service = ReplayService()
app = FastAPI(title="PostgreSQL telemetry replay")


@app.on_event("startup")
def start_service() -> None:
    service.start()


@app.on_event("shutdown")
def stop_service() -> None:
    service.stop()


@app.get("/health")
def health(response: Response) -> dict:
    status = service.status()
    if not service.healthy():
        response.status_code = 503
    return {"status": "ok" if service.healthy() else "unavailable", "stream": status}


@app.get("/status")
def get_status() -> dict:
    return service.status()


@app.put("/config")
def update_config(request: ConfigUpdate) -> dict:
    try:
        updated = service.update(request)
    except (TypeError, ValueError) as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return asdict(updated)
