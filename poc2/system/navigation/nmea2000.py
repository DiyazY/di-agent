"""Minimal NMEA 2000 (CAN bus) emulation for the navigation simulator.

Implements the PGN encoders and bus framing used by the simulated sensors,
plus an Actisense-ASCII (N2K ASCII) TCP gateway that plays the role a real
NMEA2000<->Ethernet gateway (Actisense NGT-1, Yacht Devices YDEN) plays on a
vessel: every CAN frame on the bus is forwarded to connected TCP clients as
one ASCII line, so the raw bus traffic can be observed with `nc` or decoded
with canboat's `analyzer`.

ASCII line format (one CAN frame per line, canboat plain-format compatible):

    HH:MM:SS.mmm <prio> <pgn> <src> <dst> <len> <b0> <b1> ... <bN>

    12:34:56.789 2 129025 10 255 8 40 1f 8c 2a 80 9a 06 05

PGN payloads are encoded little-endian with the standard scaling and the
"data not available" sentinels from the NMEA 2000 specification. Payloads
longer than 8 bytes (e.g. PGN 128275 Distance Log) are fragmented with the
standard fast-packet protocol: frame 0 carries (seq<<5)|0 and the total
length, subsequent frames carry (seq<<5)|frame_index and 7 data bytes each.
"""

import socket
import struct
import threading
from datetime import datetime, timezone
from math import ceil, pi

# PGNs used by the simulated sensor suite.
PGN_VESSEL_HEADING = 127250
PGN_RATE_OF_TURN = 127251
PGN_ATTITUDE = 127257
PGN_WATER_DEPTH = 128267
PGN_POSITION_RAPID = 129025
PGN_COG_SOG_RAPID = 129026
PGN_TIME_DATE = 129033
PGN_DISTANCE_LOG = 128275
PGN_WIND_DATA = 130306

DEG_TO_RAD = pi / 180.0
KNOTS_TO_M_S = 1852.0 / 3600.0

# "Data not available" sentinels per field width.
NA_U8 = 0xFF
NA_U16 = 0xFFFF
NA_I16 = 0x7FFF
NA_U32 = 0xFFFFFFFF

# Wind/heading reference codes (lower bits of the reference field).
REFERENCE_TRUE_GROUND = 0
REFERENCE_MAGNETIC = 1
REFERENCE_APPARENT = 2
REFERENCE_TRUE_WATER = 3


def _scaled(value: float | None, resolution: float, lo: int, hi: int, na: int) -> int:
    """Scale a physical value to its raw integer representation, clamping to
    the field width and mapping None to the not-available sentinel."""
    if value is None:
        return na
    raw = round(value / resolution)
    return max(lo, min(hi, raw))


def encode_position_rapid(lat_deg: float, lon_deg: float) -> bytes:
    """PGN 129025 Position, Rapid Update: lat/lon as int32, 1e-7 deg."""
    return struct.pack(
        "<ii",
        _scaled(lat_deg, 1e-7, -(2**31), 2**31 - 1, 0x7FFFFFFF),
        _scaled(lon_deg, 1e-7, -(2**31), 2**31 - 1, 0x7FFFFFFF),
    )


def encode_cog_sog_rapid(
    sid: int, cog_deg: float | None, sog_m_s: float | None, reference: int = REFERENCE_TRUE_GROUND
) -> bytes:
    """PGN 129026 COG & SOG, Rapid Update: COG 1e-4 rad, SOG 0.01 m/s."""
    cog_raw = _scaled(
        None if cog_deg is None else cog_deg * DEG_TO_RAD, 1e-4, 0, 0xFFFE, NA_U16
    )
    sog_raw = _scaled(sog_m_s, 0.01, 0, 0xFFFE, NA_U16)
    ref_byte = (reference & 0x03) | 0xFC  # upper bits reserved, set to 1
    return struct.pack("<BBHHH", sid & 0xFF, ref_byte, cog_raw, sog_raw, NA_U16)


def encode_heading(
    sid: int,
    heading_deg: float | None,
    deviation_deg: float | None = None,
    variation_deg: float | None = None,
    reference: int = REFERENCE_MAGNETIC,
) -> bytes:
    """PGN 127250 Vessel Heading: heading/deviation/variation as 1e-4 rad."""
    to_raw = lambda v: _scaled(None if v is None else v * DEG_TO_RAD, 1e-4, -(2**15), 2**15 - 1, NA_I16)  # noqa: E731
    heading_raw = _scaled(
        None if heading_deg is None else heading_deg * DEG_TO_RAD, 1e-4, 0, 0xFFFE, NA_U16
    )
    ref_byte = (reference & 0x03) | 0xFC
    return struct.pack(
        "<BHhhB", sid & 0xFF, heading_raw, to_raw(deviation_deg), to_raw(variation_deg), ref_byte
    )


def encode_rate_of_turn(sid: int, rot_deg_per_min: float | None) -> bytes:
    """PGN 127251 Rate of Turn: int32 at (1e-3/32) deg/s per bit."""
    resolution_deg_s = 1e-3 / 32.0
    rot_raw = _scaled(
        None if rot_deg_per_min is None else rot_deg_per_min / 60.0,
        resolution_deg_s,
        -(2**31),
        2**31 - 1,
        0x7FFFFFFF,
    )
    return struct.pack("<Bi", sid & 0xFF, rot_raw) + b"\xff\xff\xff"


def encode_attitude(
    sid: int, yaw_deg: float | None, pitch_deg: float | None, roll_deg: float | None
) -> bytes:
    """PGN 127257 Attitude: yaw/pitch/roll as int16, 1e-4 rad."""
    to_raw = lambda v: _scaled(None if v is None else v * DEG_TO_RAD, 1e-4, -(2**15), 2**15 - 1, NA_I16)  # noqa: E731
    return struct.pack(
        "<BhhhB", sid & 0xFF, to_raw(yaw_deg), to_raw(pitch_deg), to_raw(roll_deg), NA_U8
    )


def encode_wind(
    sid: int, speed_m_s: float | None, angle_deg: float | None, reference: int = REFERENCE_APPARENT
) -> bytes:
    """PGN 130306 Wind Data: speed 0.01 m/s, angle 1e-4 rad."""
    speed_raw = _scaled(speed_m_s, 0.01, 0, 0xFFFE, NA_U16)
    angle_raw = _scaled(
        None if angle_deg is None else angle_deg * DEG_TO_RAD, 1e-4, 0, 0xFFFE, NA_U16
    )
    return struct.pack("<BHHBB", sid & 0xFF, speed_raw, angle_raw, reference & 0xFF, NA_U8)


def encode_water_depth(
    sid: int, depth_m: float | None, offset_m: float | None = 0.0, range_m: float | None = None
) -> bytes:
    """PGN 128267 Water Depth: depth 0.01 m, transducer offset 0.001 m."""
    depth_raw = _scaled(depth_m, 0.01, 0, 0xFFFFFFFE, NA_U32)
    offset_raw = _scaled(offset_m, 0.001, -(2**15), 2**15 - 1, NA_I16)
    range_raw = _scaled(range_m, 10.0, 0, 0xFE, NA_U8)
    return struct.pack("<BIhB", sid & 0xFF, depth_raw, offset_raw, range_raw)


def encode_time_date(days_since_epoch: int, seconds_since_midnight: float) -> bytes:
    """PGN 129033 Time & Date: date as days since 1970-01-01, time 1e-4 s."""
    return struct.pack(
        "<HI",
        max(0, min(0xFFFE, int(days_since_epoch))),
        _scaled(seconds_since_midnight, 1e-4, 0, 0xFFFFFFFE, NA_U32),
    )


def encode_distance_log(
    days_since_epoch: int, seconds_since_midnight: float, total_log_m: float, trip_log_m: float
) -> bytes:
    """PGN 128275 Distance Log: date/time + total/trip log in metres.

    14 bytes, so it always exercises the fast-packet multi-frame path.
    """
    return struct.pack(
        "<HIII",
        max(0, min(0xFFFE, int(days_since_epoch))),
        _scaled(seconds_since_midnight, 1e-4, 0, 0xFFFFFFFE, NA_U32),
        _scaled(total_log_m, 1.0, 0, 0xFFFFFFFE, NA_U32),
        _scaled(trip_log_m, 1.0, 0, 0xFFFFFFFE, NA_U32),
    )


class Nmea2000Gateway:
    """Virtual NMEA 2000 bus segment with an Actisense-ASCII TCP gateway.

    Sensors call broadcast() with an encoded PGN payload; the gateway applies
    single-frame or fast-packet framing and forwards every CAN frame as one
    ASCII line to all connected TCP clients, mirroring how an Actisense
    NGT-1 exposes a real bus.
    """

    def __init__(self, host: str = "0.0.0.0", port: int = 2597, max_clients: int = 8) -> None:
        self._host = host
        self._port = port
        self._max_clients = max_clients
        self._clients: list[socket.socket] = []
        self._lock = threading.Lock()
        self._fast_packet_seq: dict[tuple[int, int], int] = {}
        self._latest: dict[int, str] = {}
        self._server_sock: socket.socket | None = None
        self._accept_thread: threading.Thread | None = None
        self._stop_event = threading.Event()

    def start(self) -> None:
        self._server_sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self._server_sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self._server_sock.bind((self._host, self._port))
        self._server_sock.listen(self._max_clients)
        self._server_sock.settimeout(1.0)
        self._accept_thread = threading.Thread(target=self._accept_loop, daemon=True)
        self._accept_thread.start()

    def stop(self) -> None:
        self._stop_event.set()
        if self._accept_thread is not None:
            self._accept_thread.join(timeout=2)
        if self._server_sock is not None:
            self._server_sock.close()
        with self._lock:
            for client in self._clients:
                try:
                    client.close()
                except OSError:
                    pass
            self._clients.clear()

    @property
    def client_count(self) -> int:
        with self._lock:
            return len(self._clients)

    def latest_lines(self) -> list[str]:
        """Most recent ASCII frame per PGN, for REST-side debugging."""
        with self._lock:
            return [self._latest[pgn] for pgn in sorted(self._latest)]

    def broadcast(self, prio: int, pgn: int, source: int, payload: bytes, dest: int = 255) -> None:
        for frame in self._to_can_frames(prio, pgn, source, dest, payload):
            line = self._format_line(prio, pgn, source, dest, frame)
            with self._lock:
                self._latest[pgn] = line
                clients = list(self._clients)
            dead: list[socket.socket] = []
            for client in clients:
                try:
                    client.sendall(line.encode("ascii"))
                except OSError:
                    dead.append(client)
            if dead:
                with self._lock:
                    for client in dead:
                        if client in self._clients:
                            self._clients.remove(client)
                        try:
                            client.close()
                        except OSError:
                            pass

    def _accept_loop(self) -> None:
        assert self._server_sock is not None
        while not self._stop_event.is_set():
            try:
                client, _addr = self._server_sock.accept()
            except socket.timeout:
                continue
            except OSError:
                break
            with self._lock:
                if len(self._clients) >= self._max_clients:
                    client.close()
                    continue
                self._clients.append(client)

    def _to_can_frames(
        self, prio: int, pgn: int, source: int, dest: int, payload: bytes
    ) -> list[bytes]:
        if len(payload) <= 8:
            return [payload]
        # Fast-packet fragmentation: frame 0 carries the total length and 6
        # data bytes; frames 1..n carry 7 data bytes each, padded with 0xFF.
        key = (pgn, source)
        seq = self._fast_packet_seq.get(key, 0)
        self._fast_packet_seq[key] = (seq + 1) % 8
        num_frames = 1 + ceil((len(payload) - 6) / 7)
        frames: list[bytes] = []
        for index in range(num_frames):
            tag = ((seq & 0x07) << 5) | index
            if index == 0:
                chunk = payload[:6]
                frames.append(bytes([tag, len(payload)]) + chunk)
            else:
                start = 6 + (index - 1) * 7
                chunk = payload[start : start + 7]
                chunk = chunk + b"\xff" * (7 - len(chunk))
                frames.append(bytes([tag]) + chunk)
        return frames

    @staticmethod
    def _format_line(prio: int, pgn: int, source: int, dest: int, payload: bytes) -> str:
        now = datetime.now(timezone.utc)
        timestamp = now.strftime("%H:%M:%S.") + f"{now.microsecond // 1000:03d}"
        hex_bytes = " ".join(f"{byte:02x}" for byte in payload)
        return f"{timestamp} {prio} {pgn} {source} {dest} {len(payload)} {hex_bytes}\n"
