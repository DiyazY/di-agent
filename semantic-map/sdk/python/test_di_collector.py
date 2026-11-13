"""Unit tests for di_collector. Standard library only: python3 -m unittest test_di_collector."""

import io
import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import di_collector  # noqa: E402

HELLO = '{"op":"hello","protocol":1,"name":"fx","node_id":"n1","interval_ms":10000,"config":{"device":0}}'


def collect_req(seq, now=1761160000):
    return json.dumps({"op": "collect", "seq": seq, "now_unix": now})


def drive(setup, *lines):
    """Run serve over lines and return (replies as dicts, stderr text, setup's hellos)."""
    out, err = io.StringIO(), io.StringIO()
    di_collector.serve(setup, stdin=io.StringIO("".join(l + "\n" for l in lines)), stdout=out, stderr=err)
    return [json.loads(l) for l in out.getvalue().splitlines()], err.getvalue()


def sample(mt="fx", value=1.0, now=1761160000):
    return {"metric_type": mt, "value": value, "timestamp_unix": now, "unit": "u"}


class ServeTest(unittest.TestCase):
    def test_hello_passes_fields(self):
        seen = []

        def setup(hello):
            seen.append(hello)
            return lambda now: []

        replies, _ = drive(setup, HELLO)
        self.assertEqual(replies, [{"protocol": 1}])
        self.assertEqual(seen[0]["interval_ms"], 10000)
        self.assertEqual(seen[0]["config"], {"device": 0})

    def test_collect(self):
        replies, _ = drive(lambda h: (lambda now: ([sample(now=now)], ["fx", "fy"])), HELLO, collect_req(42))
        self.assertEqual(replies[1]["seq"], 42)
        self.assertEqual(replies[1]["available"], ["fx", "fy"])
        self.assertEqual(replies[1]["samples"], [sample()])

    def test_no_data_is_empty_samples_and_available_absent(self):
        replies, _ = drive(lambda h: (lambda now: None), HELLO, collect_req(3))
        self.assertEqual(replies[1], {"seq": 3, "samples": []})

    def test_available_empty_is_sent(self):
        replies, _ = drive(lambda h: (lambda now: ([], [])), HELLO, collect_req(3))
        self.assertEqual(replies[1], {"seq": 3, "samples": [], "available": []})

    def test_unhealthy_is_a_reply_not_an_error(self):
        def collect(now):
            raise di_collector.Unhealthy("daemon refused the connection")

        replies, _ = drive(lambda h: collect, HELLO, collect_req(4))
        self.assertEqual(replies[1], {"seq": 4, "samples": [], "unhealthy": "daemon refused the connection"})

    def test_collect_exception_is_error_reply_and_serving_continues(self):
        calls = []

        def collect(now):
            calls.append(now)
            if len(calls) == 1:
                raise RuntimeError("device gone")
            return [sample()]

        replies, _ = drive(lambda h: collect, HELLO, collect_req(9), collect_req(10))
        self.assertEqual(replies[1], {"seq": 9, "error": "device gone"})
        self.assertEqual(replies[2]["samples"], [sample()])

    def test_setup_refusal(self):
        def setup(hello):
            raise RuntimeError("no device")

        replies, _ = drive(setup, HELLO, collect_req(1))
        self.assertEqual(replies, [{"error": "no device"}])

    def test_setup_returning_none_is_refused(self):
        replies, _ = drive(lambda h: None, HELLO)
        self.assertEqual(replies, [{"error": "setup returned no collect function"}])

    def test_unknown_op(self):
        replies, _ = drive(lambda h: (lambda now: []), HELLO, '{"op":"flush","seq":7}')
        self.assertEqual(replies[1]["seq"], 7)
        self.assertIn("unknown op", replies[1]["error"])

    def test_collect_before_hello(self):
        replies, _ = drive(lambda h: (lambda now: []), collect_req(1))
        self.assertEqual(replies, [{"seq": 1, "error": "collect before hello"}])

    def test_eof_shuts_down(self):
        out = io.StringIO()
        di_collector.serve(lambda h: (lambda now: []), stdin=io.StringIO(""), stdout=out, stderr=io.StringIO())
        self.assertEqual(out.getvalue(), "")

    def test_nan_and_inf_dropped_with_one_stderr_line_per_type(self):
        def collect(now):
            return [sample("ok"), sample("bad", float("nan")), sample("bad", float("inf")), sample("worse", float("-inf"))]

        replies, err = drive(lambda h: collect, HELLO, collect_req(1))
        self.assertEqual(replies[1]["samples"], [sample("ok")])
        lines = err.splitlines()
        self.assertEqual(len(lines), 2, err)
        self.assertIn("dropped 2 sample(s) of 'bad'", err)
        self.assertIn("dropped 1 sample(s) of 'worse'", err)

    def test_nan_elsewhere_is_error_reply_not_invalid_json(self):
        def collect(now):
            s = sample()
            s["range"] = [0, float("nan")]
            return [s]

        replies, _ = drive(lambda h: collect, HELLO, collect_req(5), collect_req(6))
        self.assertEqual(replies[1]["seq"], 5)
        self.assertIn("error", replies[1])
        self.assertEqual(len(replies), 3)

    def test_bad_return_shapes_are_error_replies(self):
        for bad in (42, "samples", ([], [], []), ([1], None), ("x", None), ([], 7)):
            with self.subTest(bad=bad):
                replies, _ = drive(lambda h, bad=bad: (lambda now: bad), HELLO, collect_req(1), collect_req(2))
                self.assertEqual(len(replies), 3)
                self.assertEqual(replies[1]["seq"], 1)
                self.assertIn("error", replies[1])

    def test_unserialisable_sample_is_error_reply(self):
        replies, _ = drive(lambda h: (lambda now: [{"metric_type": "x", "value": 1, "when": object()}]),
                           HELLO, collect_req(1))
        self.assertEqual(replies[1]["seq"], 1)
        self.assertIn("error", replies[1])

    def test_malformed_line_raises(self):
        with self.assertRaises(ValueError):
            drive(lambda h: (lambda now: []), "not json")


if __name__ == "__main__":
    unittest.main()
