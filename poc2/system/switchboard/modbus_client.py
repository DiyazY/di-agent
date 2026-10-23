"""Modbus TCP polling client mirroring how a real vessel's power management
system (PMS) supervises genset/battery controllers over a field bus.

This is the switchboard's actual source of genset/battery status for
allocation: it replaces what used to be a Kafka consumer of genset.telemetry/
battery.telemetry. Kafka is still used for everything unrelated to this
status link (consumer requests, published allocations); genset/battery still
publish to Kafka too, for the telemetry-writer/Grafana pipeline."""

import logging
import struct
import threading
import time

from pymodbus.client import ModbusTcpClient
from pymodbus.exceptions import ModbusException

logger = logging.getLogger(__name__)


def _decode_float(registers: list[int]) -> float:
    return struct.unpack(">f", struct.pack(">HH", registers[0], registers[1]))[0]


def _parse_targets(raw: str) -> list[tuple[str, str, int]]:
    """Parses "genset-1@genset-1:5020,genset-2@genset-2:5020" into
    [(id, host, port), ...]. Blank input yields no targets."""
    targets = []
    for entry in raw.split(","):
        entry = entry.strip()
        if not entry:
            continue
        target_id, _, address = entry.partition("@")
        host, _, port = address.partition(":")
        targets.append((target_id, host, int(port) if port else 5020))
    return targets


class _GensetPoller:
    registers = ("current_load_ratio", "power_kw", "speed_rpm", "co2_kg_per_s", "nox_kg_per_s")

    @staticmethod
    def read(client: ModbusTcpClient) -> dict:
        response = client.read_input_registers(address=0, count=10)
        if response.isError():
            raise ModbusException(str(response))
        regs = response.registers
        return {
            "current_load_ratio": _decode_float(regs[0:2]),
            "power_kw": _decode_float(regs[2:4]),
            "speed_rpm": _decode_float(regs[4:6]),
            "co2_kg_per_s": _decode_float(regs[6:8]),
            "nox_kg_per_s": _decode_float(regs[8:10]),
        }


class _BatteryPoller:
    registers = ("current_load_ratio", "current_charge_power_kw", "soc", "supply_power_kw")

    @staticmethod
    def read(client: ModbusTcpClient) -> dict:
        response = client.read_input_registers(address=0, count=8)
        if response.isError():
            raise ModbusException(str(response))
        regs = response.registers
        return {
            "current_load_ratio": _decode_float(regs[0:2]),
            "current_charge_power_kw": _decode_float(regs[2:4]),
            "soc": _decode_float(regs[4:6]),
            "power_kw": _decode_float(regs[6:8]),
        }


_POLLERS = {"genset": _GensetPoller, "battery": _BatteryPoller}


class ModbusPollingClient:
    """Polls a fixed list of genset/battery Modbus TCP servers on a
    background thread and keeps the last-read values per target."""

    def __init__(
        self,
        genset_targets: str,
        battery_targets: str,
        poll_interval_s: float = 2.0,
    ) -> None:
        self._targets = [
            ("genset", target_id, host, port)
            for target_id, host, port in _parse_targets(genset_targets)
        ] + [
            ("battery", target_id, host, port)
            for target_id, host, port in _parse_targets(battery_targets)
        ]
        self._poll_interval_s = poll_interval_s
        self._lock = threading.Lock()
        self._status: dict[str, dict] = {}
        self._clients: dict[str, ModbusTcpClient] = {}
        self._stop_event = threading.Event()
        self._thread: threading.Thread | None = None

    def start(self) -> None:
        if not self._targets:
            return
        self._thread = threading.Thread(target=self._run, daemon=True)
        self._thread.start()

    def stop(self) -> None:
        self._stop_event.set()
        if self._thread is not None:
            self._thread.join(timeout=self._poll_interval_s * 2)
        for client in self._clients.values():
            client.close()

    def get_status(self) -> dict:
        with self._lock:
            return {target_id: dict(value) for target_id, value in self._status.items()}

    def _run(self) -> None:
        while not self._stop_event.is_set():
            for source_type, target_id, host, port in self._targets:
                self._poll_one(source_type, target_id, host, port)
            self._stop_event.wait(self._poll_interval_s)

    def _poll_one(self, source_type: str, target_id: str, host: str, port: int) -> None:
        client = self._clients.setdefault(target_id, ModbusTcpClient(host, port=port))
        try:
            if not client.connected and not client.connect():
                raise ModbusException(f"could not connect to {host}:{port}")
            values = _POLLERS[source_type].read(client)
            with self._lock:
                self._status[target_id] = {
                    "source_type": source_type,
                    "stale": False,
                    "fetched_at": time.time(),
                    **values,
                }
        except (ModbusException, OSError) as error:
            logger.warning("Modbus poll of %s (%s:%s) failed: %s", target_id, host, port, error)
            with self._lock:
                existing = self._status.get(target_id, {"source_type": source_type})
                existing["stale"] = True
                self._status[target_id] = existing
