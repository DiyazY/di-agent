"""Forward selected live PONTOS measurements to the existing dataset pipeline."""

import json
import logging
import math
import os
import queue
import re
import threading
import time
from pathlib import Path
from typing import Any

import paho.mqtt.client as mqtt
from fastapi import FastAPI, Response
from kafka import KafkaProducer
from kafka.errors import KafkaError
from pydantic import BaseModel, ConfigDict, Field, field_validator

logging.basicConfig(level=os.environ.get("LOG_LEVEL", "INFO"))
logger = logging.getLogger("pontos-bridge")
SEGMENT_RE = re.compile(r"^[A-Za-z0-9._-]+$")
SUBJECT_RE = re.compile(r"^[A-Za-z0-9._:-]+$")
VESSEL_RE = re.compile(r"^(name|imo|mmsi)_.+$")


class SignalSpec(BaseModel):
    model_config = ConfigDict(extra="forbid")
    unit: str = Field(min_length=1)
    range: tuple[float, float] | None = None

    @field_validator("unit")
    @classmethod
    def validate_unit(cls, value: str) -> str:
        if not value.strip():
            raise ValueError("unit must not be blank")
        return value.strip()

    @field_validator("range")
    @classmethod
    def validate_range(cls, value: tuple[float, float] | None) -> tuple[float, float] | None:
        if value is not None and (
            not all(math.isfinite(bound) for bound in value) or value[0] >= value[1]
        ):
            raise ValueError("range must be finite and increasing")
        return value


def default_signals() -> dict[str, SignalSpec]:
    return {
        "enginemain_speed_rpm": SignalSpec(unit="rpm"),
        "enginemain_power_kw": SignalSpec(unit="kW"),
        "enginemain_fuelcons_lph": SignalSpec(unit="L/h"),
        "positioningsystem_sog_kn": SignalSpec(unit="kn"),
        "battery_power_kw": SignalSpec(unit="kW"),
        "battery_soc_pct": SignalSpec(unit="percent", range=(0, 100)),
    }


class BridgeConfig(BaseModel):
    model_config = ConfigDict(extra="forbid")
    vessels: list[str] = Field(default_factory=list, max_length=100)
    signals: dict[str, SignalSpec] = Field(default_factory=default_signals, min_length=1)
    max_active_series: int = Field(default=2000, ge=1)
    queue_size: int = Field(default=10000, ge=1)
    max_source_age_seconds: float = Field(default=300, gt=0, allow_inf_nan=False)

    @field_validator("vessels")
    @classmethod
    def validate_vessels(cls, values: list[str]) -> list[str]:
        if any(not VESSEL_RE.fullmatch(value) or not SUBJECT_RE.fullmatch(value) for value in values):
            raise ValueError("vessels must be valid literal subject and MQTT topic segments")
        return sorted(set(values))

    @field_validator("signals")
    @classmethod
    def validate_signals(cls, values: dict[str, SignalSpec]) -> dict[str, SignalSpec]:
        if any(not SEGMENT_RE.fullmatch(tag) for tag in values):
            raise ValueError("signal tags must be valid metric-name segments")
        return values

    def subscriptions(self) -> list[tuple[str, int]]:
        return [
            (f"PONTOS_EGRESS/{vessel}/{tag}/+", 0)
            for vessel in (self.vessels or ["+"])
            for tag in sorted(self.signals)
        ]


def map_message(
    config: BridgeConfig, topic: str, payload: bytes, received_at: float
) -> dict[str, Any]:
    parts = topic.split("/")
    if len(parts) != 4 or parts[0] != "PONTOS_EGRESS" or not all(parts[1:]):
        raise ValueError("invalid PONTOS topic")
    _, vessel, tag, index = parts
    if not VESSEL_RE.fullmatch(vessel) or not SUBJECT_RE.fullmatch(vessel):
        raise ValueError("invalid PONTOS vessel identifier")
    if tag not in config.signals or (config.vessels and vessel not in config.vessels):
        raise ValueError("measurement is outside the configured selection")
    if not SEGMENT_RE.fullmatch(index):
        raise ValueError("invalid sensor index")
    raw = json.loads(payload)
    if not isinstance(raw, dict):
        raise ValueError("payload must be an object")
    raw_time, raw_value = raw["timestamp"], raw["value"]
    if isinstance(raw_time, bool) or not isinstance(raw_time, (int, float)):
        raise ValueError("timestamp must be Unix seconds")
    if isinstance(raw_value, bool) or not isinstance(raw_value, (int, float, str)):
        raise ValueError("value must be numeric")
    timestamp, value = float(raw_time), float(raw_value)
    if not math.isfinite(timestamp) or timestamp <= 0 or not math.isfinite(value):
        raise ValueError("timestamp and value must be finite")
    if timestamp > received_at + 30 or received_at - timestamp > config.max_source_age_seconds:
        raise ValueError("measurement timestamp is stale or in the future")

    run_id = f"pontos_{vessel}"
    signal = f"pontos.{tag}.i_{index}"
    if len(run_id) > 200 or len(signal) > 200:
        raise ValueError("vessel or sensor identity exceeds the dataset contract")
    spec = config.signals[tag]
    return {
        "run_id": run_id,
        "source": "pontos",
        "signal": signal,
        "value": value,
        "origin": "live",
        "timestamp": timestamp,
        "timestamp_ns": int(timestamp * 1_000_000_000),
        "t_source": timestamp,
        "metadata": {
            "subject": f"vessel:{vessel}",
            "vessel_id": vessel,
            "parameter_id": f"{tag}_{index}",
            "provider": "PONTOS-HUB / RISE",
            "license": "ODbL-1.0 / DbCL-1.0",
            "signals": {signal: spec.model_dump(mode="json", exclude_none=True)},
        },
    }


class PontosBridge:
    def __init__(self, config: BridgeConfig | None = None) -> None:
        self.config = config if config is not None else BridgeConfig.model_validate_json(
            os.environ.get("PONTOS_CONFIG", "{}")
        )
        self._queue: queue.Queue[dict[str, Any]] = queue.Queue(self.config.queue_size)
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._worker: threading.Thread | None = None
        self._mqtt: mqtt.Client | None = None
        self._mqtt_connected = False
        self._subscribed = False
        self._kafka_connected = False
        self._mqtt_error: str | None = None
        self._kafka_error: str | None = None
        self._received = 0
        self._published = 0
        self._dropped = 0
        self._series: dict[str, float] = {}

    def accept(self, topic: str, payload: bytes, received_at: float | None = None) -> bool:
        try:
            now = time.time() if received_at is None else received_at
            message = map_message(
                self.config, topic, payload, now
            )
            key = f"{message['run_id']}/{message['signal']}"
            with self._lock:
                previous = self._series.get(key)
                timestamp = message["timestamp"]
                if previous is None and len(self._series) >= self.config.max_active_series:
                    self._series = {
                        series: last for series, last in self._series.items()
                        if now - last <= self.config.max_source_age_seconds
                    }
                    if len(self._series) >= self.config.max_active_series:
                        raise ValueError("active series limit reached")
                if previous is not None and timestamp <= previous:
                    raise ValueError("duplicate or out-of-order measurement")
                self._queue.put_nowait(message)
                self._series[key] = timestamp
                self._received += 1
            return True
        except (KeyError, TypeError, ValueError, OverflowError, queue.Full) as exc:
            with self._lock:
                self._dropped += 1
            logger.warning("Dropping PONTOS measurement: %s", "queue full" if isinstance(exc, queue.Full) else exc)
            return False

    def _on_connect(self, client, userdata, flags, reason_code, properties) -> None:
        failed = reason_code != 0
        with self._lock:
            self._mqtt_connected = not failed
            self._subscribed = False
            self._mqtt_error = f"MQTT connection refused: {reason_code}" if failed else None
        if failed:
            logger.error("MQTT connection refused: %s; check access-token validity", reason_code)
            return
        result, _ = client.subscribe(self.config.subscriptions())
        if result != mqtt.MQTT_ERR_SUCCESS:
            with self._lock:
                self._mqtt_error = f"MQTT subscribe failed: {result}"
            logger.error("MQTT subscribe failed: %s", result)

    def _on_subscribe(self, client, userdata, mid, reason_codes, properties) -> None:
        failed = len(reason_codes) != len(self.config.subscriptions()) or any(
            code.value >= 128 for code in reason_codes
        )
        with self._lock:
            self._subscribed = not failed
            self._mqtt_error = "MQTT subscription refused" if failed else None
        if failed:
            logger.error("MQTT subscription refused")

    def _on_disconnect(self, client, userdata, reason_code, properties=None) -> None:
        with self._lock:
            self._mqtt_connected = False
            self._subscribed = False
            self._mqtt_error = f"MQTT disconnected: {reason_code}"
        if not self._stop.is_set():
            logger.warning("MQTT disconnected: %s; reconnecting", reason_code)

    def _on_connect_fail(self, client, userdata) -> None:
        with self._lock:
            self._mqtt_error = "MQTT connection failed; retrying"
        logger.error("MQTT connection failed; retrying")

    def _on_message(self, client, userdata, message) -> None:
        self.accept(message.topic, message.payload)

    def start(self) -> None:
        token_path = Path(os.environ.get("PONTOS_TOKEN_FILE", "/var/run/secrets/pontos/access-token"))
        token = token_path.read_text().strip()
        if not token or any(character.isspace() for character in token):
            raise ValueError("PONTOS token file must contain one non-empty access token")
        client = mqtt.Client(protocol=mqtt.MQTTv5, transport="websockets")
        client.tls_set()
        client.ws_set_options(path="/mqtt")
        client.username_pw_set("__token__", token)
        client.reconnect_delay_set(min_delay=1, max_delay=60)
        client.on_connect = self._on_connect
        client.on_subscribe = self._on_subscribe
        client.on_disconnect = self._on_disconnect
        client.on_connect_fail = self._on_connect_fail
        client.on_message = self._on_message
        client.connect_async("pontos.ri.se", 443, keepalive=60)
        self._mqtt = client
        result = client.loop_start()
        if result is not None:
            raise RuntimeError(f"MQTT network loop failed to start: {result}")
        self._worker = threading.Thread(target=self._publish, name="pontos-kafka", daemon=True)
        self._worker.start()

    def _publish(self) -> None:
        producer = None
        try:
            while not self._stop.is_set():
                try:
                    producer = KafkaProducer(
                        bootstrap_servers=os.environ.get("KAFKA_BROKERS", "kafka:9092").split(","),
                        acks="all", retries=5, max_block_ms=5000,
                        value_serializer=lambda value: json.dumps(value, allow_nan=False).encode(),
                        key_serializer=lambda value: value.encode(),
                    )
                    with self._lock:
                        self._kafka_connected = True
                        self._kafka_error = None
                    break
                except (KafkaError, OSError) as exc:
                    with self._lock:
                        self._kafka_error = str(exc)
                    logger.error("Kafka connection failed: %s; retrying in 5 seconds", exc)
                    self._stop.wait(5)
            while producer is not None and not self._stop.is_set():
                try:
                    message = self._queue.get(timeout=0.5)
                except queue.Empty:
                    continue
                try:
                    if time.time() - message["timestamp"] > self.config.max_source_age_seconds:
                        with self._lock:
                            self._dropped += 1
                        logger.warning("Dropping queued PONTOS measurement: source timestamp is stale")
                    else:
                        producer.send(
                            os.environ.get("KAFKA_TOPIC", "dataset.telemetry"),
                            key=f"{message['run_id']}:{message['signal']}", value=message,
                        ).get()
                        with self._lock:
                            self._published += 1
                            self._kafka_connected = True
                            self._kafka_error = None
                except (KafkaError, OSError) as exc:
                    with self._lock:
                        self._kafka_connected = False
                        self._kafka_error = str(exc)
                        self._dropped += 1
                    logger.error("Kafka delivery failed; delivery outcome may be unknown: %s", exc)
                finally:
                    self._queue.task_done()
        finally:
            with self._lock:
                self._kafka_connected = False
            if producer is not None:
                producer.close(timeout=5)

    def stop(self) -> None:
        self._stop.set()
        if self._mqtt is not None:
            self._mqtt.disconnect()
            self._mqtt.loop_stop()
        if self._worker is not None:
            self._worker.join(timeout=20)

    def healthy(self) -> bool:
        # Paho 1.6 has no public API for checking its loop thread's liveness.
        mqtt_thread = self._mqtt._thread if self._mqtt is not None else None
        return (
            self._worker is not None and self._worker.is_alive()
            and mqtt_thread is not None and mqtt_thread.is_alive()
        )

    def status(self) -> dict[str, Any]:
        with self._lock:
            return {
                "mqtt_connected": self._mqtt_connected,
                "subscribed": self._subscribed,
                "kafka_connected": self._kafka_connected,
                "mqtt_error": self._mqtt_error,
                "kafka_error": self._kafka_error,
                "received": self._received,
                "published": self._published,
                "dropped": self._dropped,
                "queued": self._queue.qsize(),
                "series_count": len(self._series),
            }


bridge = PontosBridge()
app = FastAPI(title="PONTOS live telemetry bridge")


@app.on_event("startup")
def start_bridge() -> None:
    bridge.start()


@app.on_event("shutdown")
def stop_bridge() -> None:
    bridge.stop()


@app.get("/health")
def health(response: Response) -> dict[str, str]:
    response.status_code = 200 if bridge.healthy() else 503
    return {"status": "ok" if bridge.healthy() else "unavailable"}


@app.get("/status")
@app.get("/ready")
def status(response: Response) -> dict[str, Any]:
    result = bridge.status()
    if not (bridge.healthy() and result["subscribed"] and result["mqtt_connected"] and result["kafka_connected"]):
        response.status_code = 503
    return result
