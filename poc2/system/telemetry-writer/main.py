import json
import os
import time

from influxdb_client import InfluxDBClient, Point, WritePrecision
from influxdb_client.client.write_api import SYNCHRONOUS
from kafka import KafkaConsumer

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "localhost:9092").split(",")
GENSET_KAFKA_TOPIC = os.environ.get("GENSET_KAFKA_TOPIC", "genset.telemetry")
PROPULSION_KAFKA_TOPIC = os.environ.get("PROPULSION_KAFKA_TOPIC", "propulsion.telemetry")
BATTERY_KAFKA_TOPIC = os.environ.get("BATTERY_KAFKA_TOPIC", "battery.telemetry")
AUXLOAD_KAFKA_TOPIC = os.environ.get("AUXLOAD_KAFKA_TOPIC", "auxload.telemetry")
SHORE_POWER_KAFKA_TOPIC = os.environ.get("SHORE_POWER_KAFKA_TOPIC", "shore_power.telemetry")
SWITCHBOARD_KAFKA_TOPIC = os.environ.get("SWITCHBOARD_KAFKA_TOPIC", "switchboard.telemetry")
KAFKA_TOPICS = [
    GENSET_KAFKA_TOPIC,
    PROPULSION_KAFKA_TOPIC,
    BATTERY_KAFKA_TOPIC,
    AUXLOAD_KAFKA_TOPIC,
    SHORE_POWER_KAFKA_TOPIC,
    SWITCHBOARD_KAFKA_TOPIC,
]
KAFKA_GROUP_ID = os.environ.get("KAFKA_GROUP_ID", "telemetry-writer")

INFLUXDB_URL = os.environ.get("INFLUXDB_URL", "http://localhost:8086")
INFLUXDB_ORG = os.environ.get("INFLUXDB_ORG", "di-agent")
INFLUXDB_BUCKET = os.environ.get("INFLUXDB_BUCKET", "telemetry")
INFLUXDB_TOKEN = os.environ.get("INFLUXDB_TOKEN", "")

NUM_CYLINDERS = 8
GENSET_CYLINDER_FIELDS = tuple(
    f"{base}_{index}"
    for base in ("cylinder_pressure_bar", "cylinder_exhaust_temp_c")
    for index in range(1, NUM_CYLINDERS + 1)
)

# Each entry maps the tag key identifying a message's source to the measurement
# name and float fields written for that source.
MESSAGE_SCHEMAS = {
    "genset_id": {
        "measurement": "genset_telemetry",
        "fields": (
            "load_ratio",
            "power_kw",
            "speed_rpm",
            "fuel_flow_kg_per_s",
            "bsfc_g_per_kwh",
            "co2_kg_per_s",
            "nox_kg_per_s",
            "air_pressure_kpa",
            "ambient_temp_c",
            "oil_temp_c",
            "oil_pressure_bar",
            "vibration_x_mm_s",
            "vibration_y_mm_s",
            "vibration_z_mm_s",
            *GENSET_CYLINDER_FIELDS,
        ),
    },
    "propulsion_id": {
        "measurement": "propulsion_telemetry",
        "fields": (
            "load_ratio",
            "power_output_kw",
            "power_input_kw",
            "allocated_power_kw",
            "speed_rpm",
        ),
    },
    "battery_id": {
        "measurement": "battery_telemetry",
        "fields": (
            "load_ratio",
            "power_kw",
            "charge_power_kw",
            "soc",
            "soc_rate_per_hour",
            "time_to_empty_hr",
            "time_to_full_hr",
        ),
    },
    "shore_power_id": {
        "measurement": "shore_power_telemetry",
        "fields": ("power_ratio", "input_power_kw", "power_kw", "losses_kw"),
    },
    "auxload_id": {
        "measurement": "auxload_telemetry",
        "fields": ("load_ratio", "power_output_kw", "power_input_kw", "allocated_power_kw"),
    },
    "consumer_id": {
        "measurement": "switchboard_telemetry",
        "fields": (
            "requested_power_kw",
            "allocated_power_kw",
        ),
    },
}

SWITCHBOARD_AGGREGATE_FIELDS = (
    "available_supply_kw",
    "total_demand_kw",
    "total_co2_kg_per_s",
    "total_nox_kg_per_s",
)


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


def _to_points(message: dict) -> list[Point]:
    tag_key = next((key for key in MESSAGE_SCHEMAS if key in message), None)
    if tag_key is None:
        raise KeyError("message has none of the known id tags: " + ", ".join(MESSAGE_SCHEMAS))
    schema = MESSAGE_SCHEMAS[tag_key]

    point = Point(schema["measurement"]).tag(tag_key, message[tag_key])
    for field in schema["fields"]:
        if field in message:
            point = point.field(field, float(message[field]))
    timestamp = int(message["timestamp"] * 1e9)
    points = [point.time(timestamp, WritePrecision.NS)]

    if tag_key == "consumer_id":
        aggregate = Point("switchboard_aggregate").tag(
            "switchboard_id", message.get("switchboard_id", "unknown")
        )
        for field in SWITCHBOARD_AGGREGATE_FIELDS:
            if field in message:
                aggregate = aggregate.field(field, float(message[field]))
        points.append(aggregate.time(timestamp, WritePrecision.NS))

    return points


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
                points = _to_points(message)
                write_api.write(bucket=INFLUXDB_BUCKET, record=points)
            except (KeyError, ValueError, TypeError) as exc:
                print(f"Skipping malformed message {message!r}: {exc}")
    finally:
        write_api.close()
        client.close()
        consumer.close()


if __name__ == "__main__":
    main()
