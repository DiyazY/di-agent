from unittest.mock import MagicMock

import pytest
from conftest import load_component_modules
from kafka.errors import KafkaTimeoutError


@pytest.mark.parametrize(
    ("component", "modules", "controller_name"),
    [
        ("genset", ["genset", "sensors", "modbus_server", "controller"], "GensetController"),
        ("battery", ["battery", "sensors", "modbus_server", "controller"], "BatteryController"),
        ("propulsion", ["propulsion", "sensors", "modbus_server", "controller"], "PropulsionController"),
        ("auxiliary-load", ["aux_load", "controller"], "AuxLoadController"),
        ("shore-power", ["shore_power", "controller"], "ShorePowerController"),
        ("switchboard", ["switchboard", "modbus_client", "controller"], "SwitchboardController"),
    ],
)
def test_kafka_send_timeout_does_not_escape_worker(component, modules, controller_name):
    loaded = load_component_modules(component, modules)
    controller = getattr(loaded["controller"], controller_name)()
    future = MagicMock()
    future.get.side_effect = KafkaTimeoutError("broker unavailable")
    producer = MagicMock()
    producer.send.return_value = future
    controller._producer = producer

    controller._send("test.topic", key="test", value={})

    future.get.assert_called_once_with(timeout=5)
