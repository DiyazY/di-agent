import unittest
from unittest.mock import patch

from pydantic import ValidationError

from main import (
    ConfigUpdate,
    ReplayService,
    SignalProfile,
    StreamConfig,
    synthetic_records,
    time_steps,
)


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


if __name__ == "__main__":
    unittest.main()
