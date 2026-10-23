"""Modbus TCP interface mirroring the real-world link between a propulsion
drive controller and a vessel's power management system: the PMS reads
status over input registers and writes the load setpoint over a holding
register, instead of (or alongside) a REST/Kafka pipeline.

Register map (all registers are big-endian IEEE-754 float32 across 2 words,
zero-based addressing):

Holding registers (function code 3/6/16, read/write):
  0-1: target_load_ratio (0.0-1.0)

Coils (function code 1/5):
  0: anomaly_enabled

Input registers (function code 4, read-only):
  0-1: current_load_ratio (achieved, 0.0-1.0)
  2-3: allocated_power_kw
  4-5: speed_rpm
"""

import logging
import struct
import threading

from pymodbus.datastore import (
    ModbusSequentialDataBlock,
    ModbusServerContext,
    ModbusSlaveContext,
)
from pymodbus.server import StartTcpServer

logger = logging.getLogger(__name__)

TARGET_LOAD_RATIO_ADDR = 0
ANOMALY_ENABLED_ADDR = 0
CURRENT_LOAD_RATIO_ADDR = 0
ALLOCATED_POWER_KW_ADDR = 2
SPEED_RPM_ADDR = 4


def _encode_float(value: float) -> list[int]:
    return list(struct.unpack(">HH", struct.pack(">f", value)))


def _decode_float(registers: list[int]) -> float:
    return struct.unpack(">f", struct.pack(">HH", registers[0], registers[1]))[0]


class _CallbackDataBlock(ModbusSequentialDataBlock):
    """Sequential data block that invokes on_write for master-originated
    writes, but stays silent for our own periodic register sync."""

    def __init__(self, values: list[int], on_write) -> None:
        super().__init__(0, values)
        self._on_write = on_write
        self._syncing = False

    def setValues(self, address, values) -> None:
        super().setValues(address, values)
        if not self._syncing:
            self._on_write(address, values)

    def sync(self, address, values) -> None:
        self._syncing = True
        try:
            self.setValues(address, values)
        finally:
            self._syncing = False


class PropulsionModbusServer:
    """Runs a Modbus TCP server backed by the given PropulsionController, in
    addition to (not instead of) the existing REST/Kafka interfaces."""

    def __init__(self, controller, host: str = "0.0.0.0", port: int = 5020) -> None:
        self._controller = controller
        self._host = host
        self._port = port

        self._holding = _CallbackDataBlock([0] * 8, self._on_holding_write)
        self._coils = _CallbackDataBlock([0] * 8, self._on_coil_write)
        self._input = ModbusSequentialDataBlock(0, [0] * 8)
        context = ModbusServerContext(
            slaves=ModbusSlaveContext(hr=self._holding, co=self._coils, ir=self._input, zero_mode=True),
            single=True,
        )
        self._context = context
        self._server_thread: threading.Thread | None = None

    def start(self) -> None:
        self._holding.sync(TARGET_LOAD_RATIO_ADDR, _encode_float(0.0))
        self._server_thread = threading.Thread(
            target=StartTcpServer,
            kwargs={"context": self._context, "address": (self._host, self._port)},
            daemon=True,
        )
        self._server_thread.start()

    def sync_from_status(self, status: dict) -> None:
        """Pushes the controller's current state into the register map; called
        periodically from the controller's own telemetry loop."""
        self._holding.sync(TARGET_LOAD_RATIO_ADDR, _encode_float(status["target_load_ratio"]))
        self._coils.sync(ANOMALY_ENABLED_ADDR, [1 if status["anomaly_enabled"] else 0])
        self._input.setValues(CURRENT_LOAD_RATIO_ADDR, _encode_float(status["current_load_ratio"]))
        self._input.setValues(
            ALLOCATED_POWER_KW_ADDR, _encode_float(status["allocated_power_kw"])
        )
        self._input.setValues(SPEED_RPM_ADDR, _encode_float(status["speed_rpm"]))

    def _on_holding_write(self, address: int, values: list[int]) -> None:
        if address == TARGET_LOAD_RATIO_ADDR:
            try:
                load_ratio = max(0.0, min(1.0, _decode_float(values)))
                self._controller.set_target_load_ratio(load_ratio)
            except (ValueError, struct.error) as error:
                logger.warning("Ignoring invalid Modbus load_ratio write: %s", error)

    def _on_coil_write(self, address: int, values: list[int]) -> None:
        if address == ANOMALY_ENABLED_ADDR and values:
            self._controller.set_anomaly_enabled(bool(values[0]))
