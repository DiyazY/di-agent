import json
import unittest

from fastapi import HTTPException

from main import DatasetCollector


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
        self.assertEqual(sample["unit"], "unknown")
        self.assertNotIn("range", sample)
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

    def test_requires_hello_before_collect(self):
        collector = DatasetCollector()
        with self.assertRaises(HTTPException):
            collector.collect({"seq": 1, "now_unix": 1761200000})


if __name__ == "__main__":
    unittest.main()
