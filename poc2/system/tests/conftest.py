import importlib
import sys
from pathlib import Path

SYSTEM_DIR = Path(__file__).resolve().parent.parent

COMMON_MODULE_NAMES = [
    "controller",
    "sensors",
    "modbus_server",
    "modbus_client",
    "main",
    "battery",
    "genset",
    "propulsion",
    "shore_power",
    "aux_load",
    "switchboard",
    "vessel",
    "nmea2000",
]


def load_component_modules(component_name: str, module_names: list[str]):
    component_dir = str(SYSTEM_DIR / component_name)
    # Remove collided modules from sys.modules to avoid stale module cache
    for name in COMMON_MODULE_NAMES:
        sys.modules.pop(name, None)

    # Clean up any duplicate telemetry_writer prometheus metrics if reloading
    try:
        from prometheus_client import REGISTRY
        for collector in list(REGISTRY._collector_to_names.keys()):
            names = REGISTRY._collector_to_names[collector]
            if any(n.startswith("telemetry_writer_") for n in names):
                REGISTRY.unregister(collector)
    except Exception:
        pass

    # Put component directory at the top of sys.path
    if component_dir in sys.path:
        sys.path.remove(component_dir)
    sys.path.insert(0, component_dir)

    loaded = {}
    for mod_name in module_names:
        loaded[mod_name] = importlib.import_module(mod_name)
    return loaded

