from unittest.mock import MagicMock

import pytest
from conftest import load_component_modules


@pytest.mark.parametrize(
    ("component", "modules", "server_name", "addresses"),
    [
        ("genset", ["genset", "sensors", "modbus_server"], "GensetModbusServer", [0]),
        ("battery", ["battery", "sensors", "modbus_server"], "BatteryModbusServer", [0, 2]),
        ("propulsion", ["propulsion", "sensors", "modbus_server"], "PropulsionModbusServer", [0]),
    ],
)
def test_single_register_float_write_is_ignored(component, modules, server_name, addresses):
    loaded = load_component_modules(component, modules)
    modbus_module = loaded["modbus_server"]
    controller = MagicMock()
    server = getattr(modbus_module, server_name)(controller)

    for address in addresses:
        server._on_holding_write(address, [123])

    controller.set_target_load_ratio.assert_not_called()
    controller.set_target_charge_power_kw.assert_not_called()
    with pytest.raises(ValueError, match="requires two registers"):
        modbus_module._decode_float([123])
