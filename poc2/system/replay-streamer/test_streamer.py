import json
import unittest
from unittest.mock import MagicMock, patch

from pydantic import ValidationError
from starlette.testclient import TestClient

from conftest import load_component_modules

replay_modules = load_component_modules("replay-streamer", ["main"])
ConfigUpdate = replay_modules["main"].ConfigUpdate
ReplayService = replay_modules["main"].ReplayService
SignalProfile = replay_modules["main"].SignalProfile
StreamConfig = replay_modules["main"].StreamConfig
synthetic_records = replay_modules["main"].synthetic_records
time_steps = replay_modules["main"].time_steps
wait_for_send_futures = replay_modules["main"]._wait_for_send_futures
collector_metadata = replay_modules["main"]._collector_metadata
replay = replay_modules["main"]


class SyntheticRecordsTests(unittest.TestCase):
    def setUp(self):
        self.config = StreamConfig(
            mode="synthetic",
            run_id="test-run",
            interval_ms=100,
            points_per_interval=2,
            loop=False,
            random_seed=7,
        )
        self.profiles = [
            SignalProfile("short", "pressure", -2, 2, 0, 0.5, 2, 0, 1),
            SignalProfile("long_3", "temperature", 10, 10, 10, 0, 3, 0, 2),
        ]

    def test_streams_all_sources_aligned_and_shorter_ends_first(self):
        metadata = {"rpm": "1000"}
        with patch("main.load_profiles", return_value=(self.profiles, metadata)):
            records = list(synthetic_records(self.config))

        self.assertEqual(len(records), 5)
        self.assertEqual(
            [(record["t_source"], record["source"]) for record in records],
            [(0, "long_3"), (0, "short"), (1, "long_3"), (1, "short"), (2, "long_3")],
        )
        self.assertTrue(
            all(-2 <= record["value"] <= 2 for record in records if record["signal"] == "pressure")
        )
        self.assertTrue(all(record["metadata"] == metadata for record in records))
        self.assertTrue(all(record["origin"] == "synthetic" for record in records))

    def test_time_steps_group_samples_sharing_a_time(self):
        with patch("main.load_profiles", return_value=(self.profiles, {})):
            steps = list(time_steps(synthetic_records(self.config)))

        self.assertEqual([len(step) for step in steps], [2, 2, 1])
        self.assertEqual([step[0]["t_source"] for step in steps], [0, 1, 2])

    def test_generation_is_reproducible_from_seed(self):
        with patch("main.load_profiles", return_value=(self.profiles, {})):
            first = list(synthetic_records(self.config))
            second = list(synthetic_records(self.config))

        self.assertEqual([record["value"] for record in first], [record["value"] for record in second])


class StreamConfigurationTests(unittest.TestCase):
    def test_rejects_out_of_range_pacing_values(self):
        with self.assertRaises(ValidationError):
            ConfigUpdate(interval_ms=9)
        with self.assertRaises(ValidationError):
            ConfigUpdate(points_per_interval=10_001)

    def test_runtime_update_ignores_nulls_and_changes_mode(self):
        service = ReplayService()
        config = service.update(ConfigUpdate(mode="real", interval_ms=250, run_id=None))

        self.assertEqual(config.mode, "real")
        self.assertEqual(config.interval_ms, 250)
        self.assertEqual(config.run_id, "1000rpm_150kW_0001")
        self.assertEqual(config.points_per_interval, 1)


class ProducerFutureTests(unittest.TestCase):
    def test_waits_for_every_future_before_raising_send_failure(self):
        failed = MagicMock()
        failed.get.side_effect = RuntimeError("broker unavailable")
        succeeded = MagicMock()

        with self.assertRaisesRegex(RuntimeError, "broker unavailable"):
            wait_for_send_futures([failed, succeeded])

        failed.get.assert_called_once_with(timeout=5)
        succeeded.get.assert_called_once_with(timeout=5)


class CollectorMetadataTests(unittest.TestCase):
    def setUp(self):
        self.profiles = [
            SignalProfile("short", "Speed", 900, 1100, 1000, 20, 2, 0, 1),
            SignalProfile("long_3", "Speed", 800, 1200, 1000, 30, 2, 0, 1),
            SignalProfile("short", "constant", 10, 10, 10, 0, 2, 0, 1),
        ]

    def test_native_ranges_span_all_sources_and_pad_constant_signals(self):
        metadata = {"Bore": "200", "CycleDegrees": "720"}
        with self.assertLogs("replay-streamer", level="WARNING"):
            result = collector_metadata(self.profiles, metadata)

        self.assertEqual(result["Bore"], "200")
        self.assertNotIn("signals", metadata)
        speed = result["signals"]["Speed"]
        self.assertEqual(speed["unit"], "dataset-native")
        self.assertEqual(speed["range"], [800, 1200])
        self.assertEqual(speed["range_source"], "observed")
        self.assertEqual(result["signals"]["constant"]["range"], [9, 11])

    def test_preserves_explicit_json_and_flattened_physical_metadata(self):
        declarations = [
            {"signals": {"Speed": {"unit": "rpm", "range": [0, 6000]}}},
            {"signals": json.dumps({"Speed": {"unit": "rpm", "range": [0, 6000]}})},
            {"Speed.unit": "rpm", "Speed.range": "[0, 6000]"},
            {"Speed_unit": "rpm", "Speed_range": "[0, 6000]"},
            {"signals": {"Speed": json.dumps({"unit": "rpm", "minimum": 0, "maximum": 6000})}},
        ]
        for metadata in declarations:
            with self.subTest(metadata=metadata):
                result = collector_metadata(self.profiles[:1], metadata)["signals"]["Speed"]
                self.assertEqual(result["unit"], "rpm")
                self.assertEqual(result["range"], [0, 6000])
                self.assertNotIn("range_source", result)

    def test_does_not_replace_invalid_explicit_metadata(self):
        declarations = [
            {"unit": "", "range": [0, 6000]},
            {"unit": "rpm", "range": [6000, 0]},
            {"unit": "rpm", "range": [0, float("inf")]},
            {"unit": "rpm", "range": [0]},
        ]
        for details in declarations:
            with self.subTest(details=details), self.assertRaises(ValueError):
                collector_metadata(self.profiles[:1], {"signals": {"Speed": details}})
        with self.assertRaises(ValueError):
            collector_metadata(self.profiles, {"signals": "invalid JSON"})

    def test_partial_declarations_fill_only_missing_fields(self):
        declarations = [
            ({"unit": "rpm"}, "rpm", [900, 1100]),
            ({"range": [0, 6000]}, "dataset-native", [0, 6000]),
        ]
        for details, unit, bounds in declarations:
            with self.subTest(details=details), self.assertLogs("replay-streamer", level="WARNING"):
                result = collector_metadata(self.profiles[:1], {"signals": {"Speed": details}})
            self.assertEqual(result["signals"]["Speed"]["unit"], unit)
            self.assertEqual(result["signals"]["Speed"]["range"], bounds)

    def test_rejects_nonfinite_observed_ranges(self):
        profile = SignalProfile("short", "Speed", 0, float("inf"), 0, 0, 2, 0, 1)
        with self.assertRaisesRegex(ValueError, "invalid observed range"):
            collector_metadata([profile], {})


class ReplayMetricsTests(unittest.TestCase):
    def test_metrics_endpoint_reports_http_requests(self):
        with patch.object(replay.service, "start"), patch.object(replay.service, "stop"):
            with TestClient(replay.app) as client:
                self.assertEqual(client.get("/status").status_code, 200)
                response = client.get("/metrics")

        self.assertEqual(response.status_code, 200)
        self.assertIn("http_requests_total", response.text)


if __name__ == "__main__":
    unittest.main()
