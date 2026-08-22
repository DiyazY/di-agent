import json
import os
import time

from influxdb_client import InfluxDBClient, Point, WritePrecision
from influxdb_client.client.write_api import SYNCHRONOUS
from kafka import KafkaConsumer

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "localhost:9092").split(",")
KAFKA_TOPIC = os.environ.get("KAFKA_TOPIC", "genset.telemetry")
PROPULSION_KAFKA_TOPIC = os.environ.get("PROPULSION_KAFKA_TOPIC", "propulsion.telemetry")
KAFKA_TOPICS = [KAFKA_TOPIC, PROPULSION_KAFKA_TOPIC]
KAFKA_GROUP_ID = os.environ.get("KAFKA_GROUP_ID", "telemetry-writer")

INFLUXDB_URL = os.environ.get("INFLUXDB_URL", "http://localhost:8086")
INFLUXDB_ORG = os.environ.get("INFLUXDB_ORG", "di-agent")
INFLUXDB_BUCKET = os.environ.get("INFLUXDB_BUCKET", "genset-telemetry")
INFLUXDB_TOKEN = os.environ.get("INFLUXDB_TOKEN", "")

# Each entry maps the tag key identifying a message's source to the measurement
# name and float fields written for that source.
MESSAGE_SCHEMAS = {
    "genset_id": {
        "measurement": "genset_telemetry",
        "fields": ("load_ratio", "power_kw", "fuel_flow_kg_per_s", "bsfc_g_per_kwh"),
    },
    "propulsion_id": {
        "measurement": "propulsion_telemetry",
        "fields": ("load_ratio", "power_output_kw", "power_input_kw"),
    },
}


def _make_consumer() -> KafkaConsumer:
    while True:
        try:
            return KafkaConsumer(
                *KAFKA_TOPICS,
                bootstrap_servers=KAFKA_BROKERS,
                group_id=KAFKA_GROUP_ID,
                value_deserializer=lambda v: json.loads(v.decode("utf-8")),
                key_deserializer=lambda k: k.decode("utf-8") if k is not None else None,
                auto_offset_reset="latest",
            )
        except Exception as exc:  # noqa: BLE001 - broker may not be up yet
            print(f"Kafka brokers {KAFKA_BROKERS} not available yet ({exc}), retrying in 5s ...")
            time.sleep(5)


def _to_point(message: dict) -> Point:
    tag_key = next((key for key in MESSAGE_SCHEMAS if key in message), None)
    if tag_key is None:
        raise KeyError("message has none of the known id tags: " + ", ".join(MESSAGE_SCHEMAS))
    schema = MESSAGE_SCHEMAS[tag_key]

    point = Point(schema["measurement"]).tag(tag_key, message[tag_key])
    for field in schema["fields"]:
        if field in message:
            point = point.field(field, float(message[field]))
    return point.time(int(message["timestamp"] * 1e9), WritePrecision.NS)


def main() -> None:
    consumer = _make_consumer()
    client = InfluxDBClient(url=INFLUXDB_URL, token=INFLUXDB_TOKEN, org=INFLUXDB_ORG)
    write_api = client.write_api(write_options=SYNCHRONOUS)

    print(f"Consuming {KAFKA_TOPICS} from {KAFKA_BROKERS}, writing to {INFLUXDB_URL} "
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
