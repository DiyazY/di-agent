import json
import os
import time

from influxdb_client import InfluxDBClient, Point, WritePrecision
from influxdb_client.client.write_api import SYNCHRONOUS
from kafka import KafkaConsumer

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "localhost:9092").split(",")
KAFKA_TOPIC = os.environ.get("KAFKA_TOPIC", "genset.telemetry")
KAFKA_GROUP_ID = os.environ.get("KAFKA_GROUP_ID", "telemetry-writer")

INFLUXDB_URL = os.environ.get("INFLUXDB_URL", "http://localhost:8086")
INFLUXDB_ORG = os.environ.get("INFLUXDB_ORG", "di-agent")
INFLUXDB_BUCKET = os.environ.get("INFLUXDB_BUCKET", "genset-telemetry")
INFLUXDB_TOKEN = os.environ.get("INFLUXDB_TOKEN", "")

# Fields written as floats into the "genset_telemetry" measurement, tagged by genset_id.
MEASUREMENT = "genset_telemetry"
FIELD_KEYS = ("load_ratio", "power_kw", "fuel_flow_kg_per_s", "bsfc_g_per_kwh")


def _make_consumer() -> KafkaConsumer:
    while True:
        try:
            return KafkaConsumer(
                KAFKA_TOPIC,
                bootstrap_servers=KAFKA_BROKERS,
                group_id=KAFKA_GROUP_ID,
                value_deserializer=lambda v: json.loads(v.decode("utf-8")),
                key_deserializer=lambda k: k.decode("utf-8") if k is not None else None,
                auto_offset_reset="latest",
                api_version_auto_timeout_ms=30000,
            )
        except Exception as exc:  # noqa: BLE001 - broker may not be up yet
            print(f"Kafka brokers {KAFKA_BROKERS} not available yet ({exc}), retrying in 5s ...")
            time.sleep(5)


def _to_point(message: dict) -> Point:
    point = Point(MEASUREMENT).tag("genset_id", message["genset_id"])
    for field in FIELD_KEYS:
        if field in message:
            point = point.field(field, float(message[field]))
    return point.time(int(message["timestamp"] * 1e9), WritePrecision.NS)


def main() -> None:
    consumer = _make_consumer()
    client = InfluxDBClient(url=INFLUXDB_URL, token=INFLUXDB_TOKEN, org=INFLUXDB_ORG)
    write_api = client.write_api(write_options=SYNCHRONOUS)

    print(f"Consuming '{KAFKA_TOPIC}' from {KAFKA_BROKERS}, writing to {INFLUXDB_URL} "
          f"(org={INFLUXDB_ORG}, bucket={INFLUXDB_BUCKET})")

    try:
        for record in consumer:
            message = record.value
            try:
                point = _to_point(message)
                write_api.write(bucket=INFLUXDB_BUCKET, record=point)
            except (KeyError, ValueError, TypeError) as exc:
                print(f"Skipping malformed message {message!r}: {exc}")
    finally:
        write_api.close()
        client.close()
        consumer.close()


if __name__ == "__main__":
    main()
