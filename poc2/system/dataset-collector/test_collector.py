import json
import time
import unittest
from unittest.mock import patch

from fastapi import HTTPException

from conftest import load_component_modules

collector_module = load_component_modules("dataset-collector", ["main"])["main"]
DatasetCollector = collector_module.DatasetCollector


class DatasetCollectorTests(unittest.TestCase):
    def setUp(self):
        self.collector = DatasetCollector()
        self.collector.hello({
            "protocol": 1,
            "name": "dataset",
            "node_id": "agent-1",
            "interval_ms": 5000,
            "config": {},
        })

    def add(self, *, run_id="run-1", signal="Speed", source="short", value=1.0, origin="real"):
        return self.collector.ingest_json(
            json.dumps({
                "run_id": run_id,
                "signal": signal,
                "source": source,
                "value": value,
                "origin": origin,
                "metadata": {
                    "signals": {
                        signal: {"unit": "rpm", "range": [0, 6000]},
                    },
                },
            })
        )

    def test_aggregates_all_events_per_signal_for_one_agent_tick(self):
        self.assertTrue(self.add(value=1))
        self.assertTrue(self.add(value=3, source="long_333000"))

        reply = self.collector.collect({"seq": 42, "now_unix": 1761200000})

        self.assertEqual(reply["seq"], 42)
        self.assertNotIn("available", reply)
        self.assertEqual(len(reply["samples"]), 1)
        sample = reply["samples"][0]
        self.assertEqual(sample["value"], 2)
        self.assertEqual(sample["unit"], "rpm")
        self.assertEqual(sample["range"], [0, 6000])
        self.assertEqual(sample["subject"], "engine:run-1")
        self.assertEqual(sample["labels"]["dataset_source"], "long_333000,short")

    def test_each_run_is_a_distinct_subject_and_empty_ticks_are_valid(self):
        self.add(run_id="run-1", value=1)
        self.add(run_id="run-2", value=2)

        reply = self.collector.collect({"seq": 1, "now_unix": 1761200000})
        self.assertEqual([sample["subject"] for sample in reply["samples"]], [
            "engine:run-1", "engine:run-2",
        ])
        self.assertEqual(
            self.collector.collect({"seq": 2, "now_unix": 1761200005})["samples"],
            [],
        )

    def test_invalid_event_is_counted_and_not_returned(self):
        self.assertFalse(self.add(signal="not a metric"))
        self.assertEqual(self.collector.status()["dropped"], 1)
        self.assertEqual(
            self.collector.collect({"seq": 1, "now_unix": 1761200000})["samples"],
            [],
        )

    def test_requires_signal_unit_and_range_metadata(self):
        self.assertFalse(self.collector.ingest_json(json.dumps({
            "run_id": "run-1",
            "signal": "Speed",
            "source": "short",
            "value": 1.0,
            "origin": "real",
            "metadata": {},
        })))
        self.assertEqual(self.collector.status()["dropped"], 1)

    def test_requires_hello_before_collect(self):
        collector = DatasetCollector()
        with self.assertRaises(HTTPException):
            collector.collect({"seq": 1, "now_unix": 1761200000})

    def test_conflicting_metadata_is_logged_and_dropped_without_losing_batch(self):
        self.assertTrue(self.add(value=1000))
        message = {
            "run_id": "run-1", "signal": "Speed", "source": "short",
            "origin": "real", "value": 1100,
            "metadata": {"signals": {"Speed": {"unit": "rpm", "range": [0, 7000]}}},
        }
        with self.assertLogs("dataset-collector", level="WARNING"):
            self.assertFalse(self.collector.ingest_json(json.dumps(message)))
        self.assertEqual(self.collector.status()["dropped"], 1)
        reply = self.collector.collect({"seq": 1, "now_unix": 1761200000})
        self.assertEqual(reply["samples"][0]["value"], 1000)
        self.assertEqual(reply["samples"][0]["range"], [0, 6000])

    def test_live_declarations_cannot_change_after_a_poll(self):
        message = {
            "run_id": "pontos_vessel", "signal": "pontos.power.i_1", "source": "pontos",
            "origin": "live", "value": 12,
            "timestamp": time.time(),
            "metadata": {
                "subject": "vessel:imo_9007128",
                "signals": {"pontos.power.i_1": {"unit": "kW"}},
            },
        }
        self.assertTrue(self.collector.ingest_json(json.dumps(message)))
        sample, = self.collector.collect({"seq": 1, "now_unix": 1761200000})["samples"]
        self.assertNotIn("range", sample)
        for change in ["unit", "range"]:
            with self.subTest(change=change):
                changed = json.loads(json.dumps(message))
                changed["metadata"]["signals"]["pontos.power.i_1"][change] = (
                    "W" if change == "unit" else [0, 100]
                )
                with self.assertLogs("dataset-collector", level="WARNING"):
                    self.assertFalse(self.collector.ingest_json(json.dumps(changed)))
        self.assertEqual(self.collector.status()["dropped"], 2)
        self.assertEqual(self.collector.collect({"seq": 2, "now_unix": 1761200001})["samples"], [])

    def test_drained_runs_do_not_exhaust_subject_limit(self):
        with patch.object(collector_module, "MAX_ACTIVE_SERIES", 3):
            for run in ("a", "b", "c"):
                self.assertTrue(self.add(run_id=run))
            samples = self.collector.collect({"seq": 1, "now_unix": 1761200000})["samples"]
            self.assertEqual(len(samples), 3)
            self.assertTrue(self.add(run_id="d"))
            sample, = self.collector.collect({"seq": 2, "now_unix": 1761200001})["samples"]
            self.assertEqual(sample["subject"], "engine:d")

    def test_conflicting_subject_within_pending_aggregate_is_rejected(self):
        message = {
            "run_id": "vessel", "signal": "power", "source": "pontos",
            "origin": "live", "value": 12, "timestamp": time.time(),
            "metadata": {"subject": "vessel:first", "signals": {"power": {"unit": "kW"}}},
        }
        self.assertTrue(self.collector.ingest_json(json.dumps(message)))
        message["metadata"]["subject"] = "vessel:second"
        with self.assertLogs("dataset-collector", level="WARNING"):
            self.assertFalse(self.collector.ingest_json(json.dumps(message)))
        sample, = self.collector.collect({"seq": 1, "now_unix": int(time.time())})["samples"]
        self.assertEqual(sample["subject"], "vessel:first")

    def test_invalid_explicit_subject_is_not_replaced_by_engine_fallback(self):
        message = {
            "run_id": "pontos_vessel", "signal": "power", "source": "pontos",
            "origin": "live", "value": 12,
            "timestamp": time.time(),
            "metadata": {"subject": "vessel:invalid name", "signals": {"power": {"unit": "kW"}}},
        }
        with self.assertLogs("dataset-collector", level="WARNING"):
            self.assertFalse(self.collector.ingest_json(json.dumps(message)))

    def test_live_backlog_is_not_reported_as_a_fresh_observation(self):
        message = {
            "run_id": "pontos_vessel", "signal": "power", "source": "pontos",
            "origin": "live", "value": 12, "timestamp": time.time() - 301,
            "metadata": {"subject": "vessel:imo_9007128", "signals": {"power": {"unit": "kW"}}},
        }
        with self.assertLogs("dataset-collector", level="WARNING") as logs:
            self.assertFalse(self.collector.ingest_json(json.dumps(message)))
        self.assertIn("timestamp is stale", logs.output[0])
        self.assertEqual(self.collector.collect({"seq": 1, "now_unix": int(time.time())})["samples"], [])

    def test_live_aggregate_expiring_before_agent_poll_is_dropped(self):
        now = time.time()
        message = {
            "run_id": "pontos_vessel", "signal": "power", "source": "pontos",
            "origin": "live", "value": 12, "timestamp": now,
            "metadata": {"subject": "vessel:imo_9007128", "signals": {"power": {"unit": "kW"}}},
        }
        self.assertTrue(self.collector.ingest_json(json.dumps(message)))
        with patch("time.time", return_value=now + 301):
            with self.assertLogs("dataset-collector", level="WARNING"):
                reply = self.collector.collect({"seq": 1, "now_unix": int(now + 301)})
        self.assertEqual(reply["samples"], [])
        self.assertEqual(self.collector.status()["dropped"], 1)
        message["timestamp"] = now + 301
        with patch("time.time", return_value=now + 301):
            self.assertTrue(self.collector.ingest_json(json.dumps(message)))
            sample, = self.collector.collect({"seq": 2, "now_unix": int(now + 301)})["samples"]
        self.assertEqual(sample["value"], 12)


if __name__ == "__main__":
    unittest.main()
