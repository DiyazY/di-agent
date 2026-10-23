#!/usr/bin/env python3
"""Load the engine-test CSVs into PostgreSQL (idempotent).

Files are looked up in DATA_DIR; if DATA_URL (SharePoint share link) is set,
missing/changed files are downloaded there first. Connection settings come
from the standard PG* environment variables.

Schema:
  runs(run_id)                              one row per test run (file prefix)
  run_metadata(run_id, parameter, value)    *_metadata.csv
  timeseries(run_id, source, signal, t, value)
      one row per sample; "short" files are cycle-resolved, "long" files are
      high-rate. Column Data1_<x> is paired with its Data1_time_<x> column.

The multi-signal source CSVs are first split into one file per signal next to
them in DATA_DIR (<run>_timeseries_<source>_<signal>.csv, holding only
Data1_<signal> and Data1_time_<signal>); the split files are what get loaded.
Data that is already split (the same file names) can be placed in DATA_DIR
directly and is loaded as-is. `python ingest.py --split-only` performs just
the split step (no database needed).
"""
import csv
import http.cookiejar
import json
import os
import re
import sys
import urllib.parse
import urllib.request
from pathlib import Path

DATA_DIR = Path(os.environ.get("DATA_DIR", "/data"))
FILE_RE = re.compile(r"^(?P<run>.+?)_(?P<kind>metadata|timeseries_short|timeseries_long_\d+)\.csv$")
SPLIT_RE = re.compile(r"^(?P<run>.+?)_timeseries_(?P<source>short|long_\d+)_(?P<signal>.+)\.csv$")
SPLIT_DIR = DATA_DIR
VALUE_PREFIX = "Data1_"
TIME_PREFIX = "Data1_time_"

SCHEMA = """
CREATE TABLE IF NOT EXISTS runs (run_id text PRIMARY KEY);
CREATE TABLE IF NOT EXISTS run_metadata (
    run_id text NOT NULL REFERENCES runs, parameter text NOT NULL, value text,
    PRIMARY KEY (run_id, parameter));
CREATE TABLE IF NOT EXISTS timeseries (
    run_id text NOT NULL REFERENCES runs, source text NOT NULL,
    signal text NOT NULL, t double precision NOT NULL, value double precision);
CREATE TABLE IF NOT EXISTS ingested_files (
    name text PRIMARY KEY, size bigint NOT NULL, loaded_at timestamptz NOT NULL DEFAULT now());
"""
INDEXES = (
    "CREATE INDEX IF NOT EXISTS timeseries_idx ON timeseries (run_id, signal, t)",
    "CREATE INDEX IF NOT EXISTS timeseries_replay_idx "
    "ON timeseries (run_id, source, t, signal)",
)


def download(url):
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
    final = opener.open(url).geturl()
    parsed = urllib.parse.urlparse(final)
    folder = urllib.parse.parse_qs(parsed.query)["id"][0]
    base = f"{parsed.scheme}://{parsed.netloc}{'/'.join(folder.split('/')[:3])}/_api/web"
    req = urllib.request.Request(
        f"{base}/GetFolderByServerRelativeUrl('{urllib.parse.quote(folder, safe='')}')/Files"
        "?$select=Name,ServerRelativeUrl,Length",
        headers={"Accept": "application/json;odata=nometadata"})
    DATA_DIR.mkdir(parents=True, exist_ok=True)
    for f in json.load(opener.open(req))["value"]:
        target = DATA_DIR / f["Name"]
        if not f["Name"].lower().endswith(".csv") or (
                target.exists() and target.stat().st_size == int(f["Length"])):
            continue
        print(f"get {f['Name']}", flush=True)
        rel = urllib.parse.quote(f["ServerRelativeUrl"], safe="")
        part = target.with_suffix(".part")
        with opener.open(f"{base}/GetFileByServerRelativeUrl('{rel}')/$value") as r, open(part, "wb") as out:
            while chunk := r.read(1 << 20):
                out.write(chunk)
        part.rename(target)


def signal_pairs(header):
    """Return [(signal, value_index, time_index)] for paired Data1_ columns."""
    idx = {h: i for i, h in enumerate(header)}
    return [(h[len(VALUE_PREFIX):], i, idx[TIME_PREFIX + h[len(VALUE_PREFIX):]])
            for h, i in idx.items()
            if h.startswith(VALUE_PREFIX) and not h.startswith(TIME_PREFIX)
            and TIME_PREFIX + h[len(VALUE_PREFIX):] in idx]


def split_file(path):
    """Split one multi-signal timeseries CSV into one file per signal."""
    m = FILE_RE.match(path.name)
    prefix = f"{m['run']}_{m['kind']}"
    with path.open(newline="") as fh:
        rows = csv.reader(fh)
        pairs = signal_pairs(next(rows))
        targets = {sig: SPLIT_DIR / f"{prefix}_{sig}.csv" for sig, _, _ in pairs}
        if all(t.exists() and t.stat().st_mtime >= path.stat().st_mtime for t in targets.values()):
            return list(targets.values())
        SPLIT_DIR.mkdir(parents=True, exist_ok=True)
        parts = {sig: t.with_suffix(".part") for sig, t in targets.items()}
        outs = {sig: part.open("w", newline="") for sig, part in parts.items()}
        writers = {sig: csv.writer(out) for sig, out in outs.items()}
        for sig, writer in writers.items():
            writer.writerow([VALUE_PREFIX + sig, TIME_PREFIX + sig])
        try:
            for row in rows:
                for sig, vi, ti in pairs:
                    if len(row) > max(vi, ti) and row[ti] != "":
                        writers[sig].writerow((row[vi], row[ti]))
        finally:
            for out in outs.values():
                out.close()
        for sig, part in parts.items():
            part.replace(targets[sig])
    print(f"split {path.name} -> {len(targets)} signal file(s)", flush=True)
    return list(targets.values())


def split_all():
    for path in sorted(DATA_DIR.glob("*.csv")):
        m = FILE_RE.match(path.name)
        if m and m["kind"] != "metadata":
            split_file(path)


def load_metadata(cur, run, path):
    with path.open(newline="") as fh, cur.copy(
            "COPY run_metadata (run_id, parameter, value) FROM STDIN") as cp:
        rows = csv.reader(fh)
        next(rows)
        for row in rows:
            if row:
                cp.write_row((run, row[0], row[1] if len(row) > 1 and row[1] != "" else None))


def load_timeseries(cur, run, source, path):
    with path.open(newline="") as fh, cur.copy(
            "COPY timeseries (run_id, source, signal, t, value) FROM STDIN") as cp:
        rows = csv.reader(fh)
        header = next(rows)
        pairs = signal_pairs(header)
        for row in rows:
            for signal, vi, ti in pairs:
                if row[ti] != "":
                    cp.write_row((run, source, signal, row[ti], row[vi] or None))


def main():
    if url := os.environ.get("DATA_URL"):
        download(url)
    split_all()
    if "--split-only" in sys.argv:
        return
    import psycopg

    files = sorted(p for p in DATA_DIR.glob("*.csv") if (m := FILE_RE.match(p.name)) and m["kind"] == "metadata")
    files += sorted(p for p in DATA_DIR.glob("*.csv") if SPLIT_RE.match(p.name))
    if not files:
        sys.exit(f"no CSV files found in {DATA_DIR}")

    with psycopg.connect(autocommit=True) as conn:
        conn.execute(SCHEMA)
        done = dict(conn.execute("SELECT name, size FROM ingested_files").fetchall())
        for path in files:
            size = path.stat().st_size
            name = path.name
            split = SPLIT_RE.match(name)
            if done.get(name) == size:
                print(f"skip {name}", flush=True)
                continue
            print(f"load {name}", flush=True)
            with conn.transaction(), conn.cursor() as cur:
                if split:
                    run, source, signal = split["run"], split["source"], split["signal"]
                    cur.execute("INSERT INTO runs VALUES (%s) ON CONFLICT DO NOTHING", (run,))
                    cur.execute(
                        "DELETE FROM timeseries WHERE run_id = %s AND source = %s AND signal = %s",
                        (run, source, signal))
                    load_timeseries(cur, run, source, path)
                else:
                    run = FILE_RE.match(path.name)["run"]
                    cur.execute("INSERT INTO runs VALUES (%s) ON CONFLICT DO NOTHING", (run,))
                    cur.execute("DELETE FROM run_metadata WHERE run_id = %s", (run,))
                    load_metadata(cur, run, path)
                cur.execute(
                    "INSERT INTO ingested_files (name, size) VALUES (%s, %s) "
                    "ON CONFLICT (name) DO UPDATE SET size = EXCLUDED.size, loaded_at = now()",
                    (name, size))
        for index in INDEXES:
            conn.execute(index)
        conn.execute("ANALYZE timeseries")
    print("done", flush=True)


if __name__ == "__main__":
    main()
