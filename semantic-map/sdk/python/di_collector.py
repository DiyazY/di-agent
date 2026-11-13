"""Write a di-agent collector plugin in Python.

The agent starts the plugin once and speaks wire protocol v1 to it: one JSON
request per line on stdin, one JSON reply per line on stdout. ``serve`` runs that
loop so a plugin only says what to measure::

    import di_collector

    def setup(hello):
        # hello: {"op", "protocol", "name", "node_id", "interval_ms", "config"}
        def collect(now):
            return [{"metric_type": "queue_depth", "value": 3.0,
                     "timestamp_unix": now, "unit": "items"}]
        return collect

    di_collector.serve(setup)

``collect(now)`` is called with the request's ``now_unix`` (Unix seconds, the
timestamp a sample should carry; the SDK echoes ``seq`` itself) and returns a list of samples (``types.MetricSample`` JSON, as
``POST /ingest-sample`` takes) or a ``(samples, available)`` pair, where
``available`` lists every type the plugin can produce now (``[]`` says none);
without it the agent takes the types in ``samples``. An empty list is "no data
right now". A sample whose value is NaN or infinite cannot be sent as JSON: it is
left out of the reply and a line on stderr says so.

Raising ``Unhealthy(reason)`` in ``collect`` says the plugin's source did not
answer (its daemon is down, the device is gone): the reply carries the reason, the
agent marks the plugin unhealthy and asks again next tick.

Raising in ``setup`` refuses the hello. Raising anything else in ``collect``, or returning
something that is not a list of samples or a ``(samples, available)`` pair, is that
tick's error reply, and the agent restarts the plugin with backoff; the plugin
itself never dies of a bad tick. Leave ``node_id``, ``source`` and ``event_id``
out: the agent stamps them. Log to stderr; stdout belongs to the protocol.

Standard library only, one file: copy it beside the plugin or put its directory
on ``sys.path``. Certify the plugin with ``mapctl collector check --exec PLUGIN``.
"""

import json
import math
import sys

PROTOCOL_VERSION = 1

__all__ = ["PROTOCOL_VERSION", "Unhealthy", "serve"]


class Unhealthy(Exception):
    """Raised by collect when its source did not answer; not a fault."""


def _encode(msg):
    # allow_nan=False is the backstop: a non-finite number that got past the
    # filter raises here, inside the tick, rather than writing invalid JSON.
    return json.dumps(msg, separators=(",", ":"), allow_nan=False)


def _write(out, line):
    out.write(line + "\n")
    # The agent waits for each reply before sending anything else.
    out.flush()


def _collect_reply(collect, req, err):
    """Run one collect and encode its reply; any failure raises to the caller."""
    seq = req.get("seq", 0)
    result = collect(req.get("now_unix", 0))
    available = None
    if isinstance(result, tuple):
        if len(result) != 2:
            raise TypeError("collect returned a tuple of %d; want (samples, available)" % len(result))
        samples, available = result
    else:
        samples = result
    if samples is None:
        samples = []
    if not isinstance(samples, (list, tuple)):
        raise TypeError("collect returned %s; want a list of samples" % type(samples).__name__)
    kept, dropped = [], {}
    for s in samples:
        if s is None:
            continue
        if not isinstance(s, dict):
            raise TypeError("a sample is %s; want a dict" % type(s).__name__)
        v = s.get("value")
        if isinstance(v, float) and not math.isfinite(v):
            mt = str(s.get("metric_type", ""))
            dropped[mt] = dropped.get(mt, 0) + 1
            continue
        kept.append(s)
    for mt, n in dropped.items():
        err.write("di_collector: dropped %d sample(s) of %r with a NaN or infinite value\n" % (n, mt))
    if dropped:
        err.flush()
    reply = {"seq": seq, "samples": kept}
    if available is not None:
        if not isinstance(available, (list, tuple)):
            raise TypeError("available is %s; want a list of metric types" % type(available).__name__)
        reply["available"] = list(available)
    return _encode(reply)


def _answer(collect, req, err):
    """One collect request's reply line. Nothing a tick does escapes as an exception."""
    seq = req.get("seq", 0)
    if collect is None:
        return _encode({"seq": seq, "error": "collect before hello"})
    try:
        return _collect_reply(collect, req, err)
    except Unhealthy as e:
        return _encode({"seq": seq, "samples": [], "unhealthy": str(e) or "unhealthy"})
    except Exception as e:  # a bad tick costs that tick, not the process
        return _encode({"seq": seq, "error": str(e) or type(e).__name__})


def serve(setup, stdin=None, stdout=None, stderr=None):
    """Answer the agent until stdin closes, which is how the agent stops a plugin.

    ``setup(hello)`` is called on every hello and returns the collect function. If
    it raises or returns None, the refusal is sent and ``serve`` returns, since the
    agent will not use a plugin that refused. A line that is not a JSON object
    raises ``ValueError``: the agent never sends one.
    """
    stdin = stdin or sys.stdin
    stdout = stdout or sys.stdout
    stderr = stderr or sys.stderr
    collect = None
    for line in stdin:
        if not line.strip():
            continue
        req = json.loads(line)
        if not isinstance(req, dict):
            raise ValueError("request is not a JSON object: %r" % line)
        op = req.get("op")
        if op == "hello":
            try:
                collect = setup(req)
                if collect is None:
                    raise ValueError("setup returned no collect function")
            except Exception as e:
                _write(stdout, _encode({"error": str(e) or type(e).__name__}))
                return
            _write(stdout, _encode({"protocol": PROTOCOL_VERSION}))
        elif op == "collect":
            _write(stdout, _answer(collect, req, stderr))
        else:
            # One reply per request keeps the agent's reads in step.
            _write(stdout, _encode({"seq": req.get("seq", 0), "error": "unknown op %r" % op}))
