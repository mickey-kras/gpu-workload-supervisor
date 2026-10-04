#!/usr/bin/python3
"""Protocol peer for native Gio integration, never a GPU implementation."""
import json
import os
from pathlib import Path
import sys
import time

raw = sys.stdin.buffer.read()
request = json.loads(raw)
scenario = request["requestId"]
state = Path(os.environ["GPU_NATIVE_FIXTURE_DIR"])
(state / f"{scenario}.request").write_bytes(raw)
def wait_for_release():
    deadline = time.monotonic() + 105
    while not (state / f"{scenario}.release").exists():
        if time.monotonic() >= deadline:
            raise TimeoutError("Test client did not release fixture")
        time.sleep(0.01)
    (state / f"{scenario}.completed").touch()


response = json.dumps({"protocolVersion": 1, "requestId": scenario, "code": "busy"})
if scenario == "oversized-response":
    response += " " * (65537 - len(response))
elif scenario == "exact-response":
    response += " " * (65536 - len(response))
elif scenario == "wrong-id":
    response = response.replace("wrong-id", "different-id")
elif scenario == "malformed":
    response = "not json"
elif scenario == "empty":
    sys.exit(1)
elif scenario in ("detached", "deadline"):
    (state / f"{scenario}.admitted").touch()
    wait_for_release()
    sys.exit(0)
elif scenario == "waiting-exit":
    print(response, end="", flush=True)
    os.close(1)
    sys.stdout = open(os.devnull, "w")
    (state / f"{scenario}.admitted").touch()
    wait_for_release()
    sys.exit(0)
print(response, end="", flush=True)
