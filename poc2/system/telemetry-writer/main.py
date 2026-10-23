import json
import os
import time

from influxdb_client import InfluxDBClient, Point, WritePrecision
from influxdb_client.client.write_api import SYNCHRONOUS
from kafka import KafkaConsumer
from prometheus_client import Counter, start_http_server

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "localhost:9092").split(",")
GENSET_KAFKA_TOPIC = os.environ.get("GENSET_KAFKA_TOPIC", "genset.telemetry")
PROPULSION_KAFKA_TOPIC = os.environ.get("PROPULSION_KAFKA_TOPIC", "propulsion.telemetry")
BATTERY_KAFKA_TOPIC = os.environ.get("BATTERY_KAFKA_TOPIC", "battery.telemetry")
AUXLOAD_KAFKA_TOPIC = os.environ.get("AUXLOAD_KAFKA_TOPIC", "auxload.telemetry")
SHORE_POWER_KAFKA_TOPIC = os.environ.get("SHORE_POWER_KAFKA_TOPIC", "shore_power.telemetry")
SWITCHBOARD_KAFKA_TOPIC = os.environ.get("SWITCHBOARD_KAFKA_TOPIC", "switchboard.telemetry")
NAVIGATION_KAFKA_TOPIC = os.environ.get("NAVIGATION_KAFKA_TOPIC", "navigation.telemetry")
DATASET_KAFKA_TOPIC = os.environ.get("DATASET_KAFKA_TOPIC", "dataset.telemetry")
KAFKA_TOPICS = [
    GENSET_KAFKA_TOPIC,
    PROPULSION_KAFKA_TOPIC,
    BATTERY_KAFKA_TOPIC,
    AUXLOAD_KAFKA_TOPIC,
    SHORE_POWER_KAFKA_TOPIC,
    SWITCHBOARD_KAFKA_TOPIC,
    NAVIGATION_KAFKA_TOPIC,
    DATASET_KAFKA_TOPIC,
]
KAFKA_GROUP_ID = os.environ.get("KAFKA_GROUP_ID", "telemetry-writer")

INFLUXDB_URL = os.environ.get("INFLUXDB_URL", "http://localhost:8086")
INFLUXDB_ORG = os.environ.get("INFLUXDB_ORG", "di-agent")
INFLUXDB_BUCKET = os.environ.get("INFLUXDB_BUCKET", "telemetry")
INFLUXDB_TOKEN = os.environ.get("INFLUXDB_TOKEN", "")
LOG_EVERY_N_MESSAGES = int(os.environ.get("LOG_EVERY_N_MESSAGES", "50"))
STRICT_VALIDATION = os.environ.get("STRICT_VALIDATION", "true").lower() == "true"
METRICS_PORT = int(os.environ.get("METRICS_PORT", "8000"))

# Defaults suit the edge's plaintext local broker. Set KAFKA_SECURITY_PROTOCOL
# to SASL_SSL and KAFKA_SASL_PASSWORD to an Event Hubs connection string to
# run this same writer against Azure Event Hubs' Kafka-compatible endpoint.
KAFKA_SECURITY_PROTOCOL = os.environ.get("KAFKA_SECURITY_PROTOCOL", "PLAINTEXT")
KAFKA_SASL_MECHANISM = os.environ.get("KAFKA_SASL_MECHANISM", "PLAIN")
KAFKA_SASL_USERNAME = os.environ.get("KAFKA_SASL_USERNAME", "$ConnectionString")
KAFKA_SASL_PASSWORD = os.environ.get("KAFKA_SASL_PASSWORD", "")

MESSAGES_SEEN = Counter("telemetry_writer_messages_seen_total", "Kafka messages consumed")
MESSAGES_WRITTEN = Counter("telemetry_writer_messages_written_total", "Messages written to InfluxDB")
MESSAGES_DROPPED = Counter("telemetry_writer_messages_dropped_total", "Malformed messages dropped")

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
            "fuel_flow_kg_per_s",
            "bsfc_g_per_kwh",
            "co2_kg_per_s",
            "nox_kg_per_s",
            "lng_level",
            "lng_consumption_rate_kg_per_hr",
            "lng_time_to_empty_hr",
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
            "lever_position_pct",
            "ship_speed_knots",
            "propeller_torque_kn_m",
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
            "voltage_measured_v",
            "current_measured_a",
            "temperature_measured_c",
            "current_charge_a",
            "voltage_charge_v",
            "cycle_time_s",
            "capacity_ahr",
        ),
        "tags": ("cycle_type",),
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
    "gnss_id": {
        "measurement": "navigation_gnss",
        "fields": ("latitude_deg", "longitude_deg", "cog_deg", "sog_knots"),
        "tags": ("vessel",),
    },
    "heading_id": {
        "measurement": "navigation_heading",
        "fields": ("heading_deg", "rot_deg_per_min"),
        "tags": ("vessel",),
    },
    "wind_id": {
        "measurement": "navigation_wind",
        "fields": ("wind_speed_m_s", "wind_angle_deg"),
        "tags": ("vessel", "reference"),
    },
    "depth_id": {
        "measurement": "navigation_depth",
        "fields": ("depth_m", "offset_m"),
        "tags": ("vessel",),
    },
    "attitude_id": {
        "measurement": "navigation_attitude",
        "fields": ("yaw_deg", "pitch_deg", "roll_deg"),
        "tags": ("vessel",),
    },
}

SWITCHBOARD_AGGREGATE_FIELDS = (
    "available_supply_kw",
    "total_demand_kw",
    "total_co2_kg_per_s",
    "total_nox_kg_per_s",
)


def _make_consumer() -> KafkaConsumer:
    extra_kwargs = {}
    if KAFKA_SECURITY_PROTOCOL != "PLAINTEXT":
        extra_kwargs.update(
            security_protocol=KAFKA_SECURITY_PROTOCOL,
            sasl_mechanism=KAFKA_SASL_MECHANISM,
            sasl_plain_username=KAFKA_SASL_USERNAME,
            sasl_plain_password=KAFKA_SASL_PASSWORD,
        )
    while True:
        try:
            return KafkaConsumer(
                *KAFKA_TOPICS,
                bootstrap_servers=KAFKA_BROKERS,
                group_id=KAFKA_GROUP_ID,
                value_deserializer=lambda v: json.loads(v.decode("utf-8")),
                key_deserializer=lambda k: k.decode("utf-8") if k is not None else None,
                auto_offset_reset="latest",
                **extra_kwargs,
            )
        except Exception as exc:  # noqa: BLE001 - broker may not be up yet
            print(f"Kafka brokers {KAFKA_BROKERS} not available yet ({exc}), retrying in 5s ...")
            time.sleep(5)


def _normalize_message(message: dict) -> dict:
    if not isinstance(message, dict):
        raise TypeError("message must be a dict")
    if "payload" in message and isinstance(message["payload"], dict):
        payload = dict(message["payload"])
        payload.setdefault("source_id", message.get("source_id"))
        payload.setdefault("source_type", message.get("source_type"))
        payload.setdefault("event", message.get("event"))
        payload.setdefault("schema_version", message.get("schema_version"))
        payload.setdefault("timestamp", message.get("timestamp"))
        return payload
    return message


def _dataset_to_point(message: dict) -> Point:
    required = ("run_id", "source", "signal", "value", "timestamp")
    missing = [key for key in required if key not in message]
    if missing:
        raise KeyError("dataset message is missing: " + ", ".join(missing))
    point = (
        Point("dataset_timeseries")
        .tag("run_id", str(message["run_id"]))
        .tag("source", str(message["source"]))
        .tag("signal", str(message["signal"]))
        .tag("origin", str(message.get("origin", "unknown")))
        .field("value", float(message["value"]))
    )
    if "t_source" in message:
        point = point.field("t_source", float(message["t_source"]))
    timestamp = int(message.get("timestamp_ns", float(message["timestamp"]) * 1e9))
    return point.time(timestamp, WritePrecision.NS)


def _to_points(message: dict, topic: str | None = None) -> list[Point]:
    if topic == DATASET_KAFKA_TOPIC:
        return [_dataset_to_point(message)]
    normalized = _normalize_message(message)
    tag_key = next((key for key in MESSAGE_SCHEMAS if key in normalized), None)
    if tag_key is None:
        raise KeyError("message has none of the known id tags: " + ", ".join(MESSAGE_SCHEMAS))
    schema = MESSAGE_SCHEMAS[tag_key]

    point = Point(schema["measurement"]).tag(tag_key, normalized[tag_key])
    for tag in schema.get("tags", ()):
        if tag in normalized:
            point = point.tag(tag, normalized[tag])
    for field in schema["fields"]:
        if field in normalized:
            point = point.field(field, float(normalized[field]))
    timestamp_value = normalized.get("timestamp")
    if timestamp_value is None:
        raise KeyError("message does not contain a timestamp")
    timestamp = int(float(timestamp_value) * 1e9)
    points = [point.time(timestamp, WritePrecision.NS)]

    if tag_key == "consumer_id":
        aggregate = Point("switchboard_aggregate").tag(
            "switchboard_id", normalized.get("switchboard_id", "unknown")
        )
        for field in SWITCHBOARD_AGGREGATE_FIELDS:
            if field in normalized:
                aggregate = aggregate.field(field, float(normalized[field]))
        points.append(aggregate.time(timestamp, WritePrecision.NS))

    return points


def main() -> None:
    start_http_server(METRICS_PORT)
    consumer = _make_consumer()
    client = InfluxDBClient(url=INFLUXDB_URL, token=INFLUXDB_TOKEN, org=INFLUXDB_ORG)
    write_api = client.write_api(write_options=SYNCHRONOUS)
    stats = {"seen": 0, "written": 0, "dropped": 0}

    print(f"Consuming {KAFKA_TOPICS} from {KAFKA_BROKERS}, writing to {INFLUXDB_URL} "
          f"(org={INFLUXDB_ORG}, bucket={INFLUXDB_BUCKET})")

    try:
        for record in consumer:
            message = record.value
            stats["seen"] += 1
            MESSAGES_SEEN.inc()
            try:
                points = _to_points(message, record.topic)
                write_api.write(bucket=INFLUXDB_BUCKET, record=points)
                stats["written"] += 1
                MESSAGES_WRITTEN.inc()
            except (KeyError, ValueError, TypeError) as exc:
                stats["dropped"] += 1
                MESSAGES_DROPPED.inc()
                if STRICT_VALIDATION:
                    print(f"Dropped malformed message {message!r}: {exc}")
                else:
                    print(f"Skipping malformed message {message!r}: {exc}")
            if LOG_EVERY_N_MESSAGES and stats["seen"] % LOG_EVERY_N_MESSAGES == 0:
                print(
                    "telemetry-writer stats: "
                    f"seen={stats['seen']} written={stats['written']} dropped={stats['dropped']}"
                )
    finally:
        print(
            "telemetry-writer final stats: "
            f"seen={stats['seen']} written={stats['written']} dropped={stats['dropped']}"
        )
        write_api.close()
        client.close()
        consumer.close()


if __name__ == "__main__":
    main()
