"""Exercise peer recommendations through three real agent processes."""

import json
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path


def request(url, data=None):
    payload = None if data is None else json.dumps(data).encode()
    req = urllib.request.Request(
        url, data=payload, headers={"Content-Type": "application/json"}
    )
    with urllib.request.urlopen(req, timeout=5) as response:
        body = response.read()
        return json.loads(body) if body else None


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def main(binary):
    processes = []
    with tempfile.TemporaryDirectory(prefix="di-agent-peer-e2e-") as directory:
        root = Path(directory)
        logs = []
        try:
            nodes = []
            for name in ("source", "preferred", "alternate"):
                port = free_port()
                config = root / f"{name}.yaml"
                config.write_text(
                    f'node:\n  id: "{name}"\n  addr: "127.0.0.1:{port}"\n'
                    'sources:\n  collectors: []\n'
                    'persist:\n  file: ""\n'
                    'map:\n  proposer:\n    enabled: false\n'
                )
                log = (root / f"{name}.log").open("w+")
                logs.append(log)
                process = subprocess.Popen(
                    [binary, "-config", str(config)], stdout=log, stderr=subprocess.STDOUT,
                    cwd=Path(__file__).resolve().parent,
                )
                processes.append(process)
                url = f"http://127.0.0.1:{port}"
                deadline = time.monotonic() + 30
                while True:
                    if process.poll() is not None:
                        raise RuntimeError(f"{name} exited with {process.returncode}")
                    try:
                        request(url + "/healthz")
                        break
                    except (OSError, urllib.error.URLError):
                        if time.monotonic() >= deadline:
                            raise TimeoutError(f"{name} did not become ready")
                        time.sleep(0.1)
                nodes.append((name, url))

            for (name, url), value in zip(nodes, (0.9, 0.1, 0.4)):
                request(url + "/ingest-sample", {
                    "node_id": name, "metric_type": "cpu_utilization", "value": value,
                    "timestamp_unix": int(time.time()), "event_id": f"{name}-cpu",
                })
            costs = [request(url + "/cost?task=pod-scheduling")["ResourceCost"]
                     for _, url in nodes]
            if not costs[1] < costs[2] < costs[0]:
                raise AssertionError(f"expected preferred < alternate < source costs, got {costs}")

            source = nodes[0][1]
            peer_ids = []
            for _, url in nodes[1:]:
                peer = request(source + "/peers", {"url": url})
                peer_ids.append(peer["id"])
                request(source + f"/peers/{peer['id']}/trust", {"value": 0.8})

            context = {"TaskType": "pod-scheduling", "SourceNodeID": "source"}
            first = request(source + "/recommend", context)
            if first["PeerID"] != peer_ids[0]:
                raise AssertionError(f"expected preferred peer: {first}")
            request(source + f"/peers/{peer_ids[0]}/trust", {"value": 0.1})
            second = request(source + "/recommend", context)
            if second["PeerID"] != peer_ids[1]:
                raise AssertionError(f"expected alternate peer after trust reduction: {second}")
            request(source + f"/peers/{peer_ids[1]}/trust", {"value": 0.1})
            try:
                request(source + "/recommend", context)
            except urllib.error.HTTPError as error:
                if error.code != 409:
                    raise
                body = json.load(error)
                if ("2 peers below trust floor" not in body["error"]
                        or "0 http errors" not in body["error"]):
                    raise AssertionError(f"unexpected recommendation failure: {body}")
            else:
                raise AssertionError("recommendation succeeded with no trusted peers")
            print("Real agent peer recommendation and trust rerouting passed")
        except Exception:
            for log in logs:
                log.flush()
                log.seek(0)
                print(f"\n--- {log.name} ---\n{log.read()}", file=sys.stderr)
            raise
        finally:
            for process in processes:
                process.terminate()
            for process in processes:
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
            for log in logs:
                log.close()


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: python3 peer_e2e.py /absolute/path/to/agent")
    main(str(Path(sys.argv[1]).resolve()))
