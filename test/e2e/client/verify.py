"""E2E verification: drive Shpiel with the real huggingface_hub library and
hf CLI, exactly as a researcher's environment would — only HF_ENDPOINT set.

Expectations arrive via environment variables:
  E2E_REPO    repo id (e.g. "fixtures/e2e-model")
  E2E_COMMIT  commit SHA "main" must resolve to
  E2E_FILES   JSON: {path: {"sha256": hex, "size": n}, ...}

Prints E2E_OK as the last line on success; any assertion failure exits
non-zero.
"""

import hashlib
import json
import os
import subprocess
import sys

from huggingface_hub import HfApi, hf_hub_download, snapshot_download

ENDPOINT = os.environ["HF_ENDPOINT"]
REPO = os.environ["E2E_REPO"]
COMMIT = os.environ["E2E_COMMIT"]
FILES = json.loads(os.environ["E2E_FILES"])


def sha256_of(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def check(name, cond, detail=""):
    if not cond:
        print(f"FAIL {name}: {detail}", file=sys.stderr)
        sys.exit(1)
    print(f"ok {name}")


# 1. Repo metadata through the API client.
api = HfApi(endpoint=ENDPOINT)
info = api.model_info(REPO)
check("model_info.sha", info.sha == COMMIT, f"{info.sha} != {COMMIT}")
siblings = {s.rfilename for s in info.siblings}
check("model_info.siblings", siblings == set(FILES), f"{siblings} != {set(FILES)}")

# 2. Single-file download (hf_hub_download is what from_pretrained uses).
local = hf_hub_download(repo_id=REPO, filename="config.json")
check("hf_hub_download.content", sha256_of(local) == FILES["config.json"]["sha256"])
check("hf_hub_download.snapshot_path", f"snapshots/{COMMIT}" in local, local)

# 3. Full snapshot download, every byte verified.
snap = snapshot_download(repo_id=REPO)
for path, meta in FILES.items():
    fp = os.path.join(snap, path)
    check(f"snapshot.{path}.exists", os.path.isfile(fp), fp)
    check(f"snapshot.{path}.sha256", sha256_of(fp) == meta["sha256"])
    check(f"snapshot.{path}.size", os.path.getsize(fp) == meta["size"])

# 4. Offline reuse: the local cache written via Shpiel must satisfy
# HF_HUB_OFFLINE=1 (constants are read at import, so use a subprocess).
offline = subprocess.run(
    [
        sys.executable,
        "-c",
        (
            "from huggingface_hub import hf_hub_download;"
            f"p = hf_hub_download(repo_id={REPO!r}, filename='config.json');"
            "print(p)"
        ),
    ],
    env={**os.environ, "HF_HUB_OFFLINE": "1"},
    capture_output=True,
    text=True,
)
check("offline_reuse", offline.returncode == 0, offline.stderr[-500:])

# 5. The hf CLI itself — the literal M0 exit criterion.
cli = subprocess.run(
    ["hf", "download", REPO, "--local-dir", "/tmp/cli-download"],
    capture_output=True,
    text=True,
)
check("hf_cli.exit", cli.returncode == 0, cli.stderr[-500:])
for path in FILES:
    check(f"hf_cli.{path}", os.path.isfile(os.path.join("/tmp/cli-download", path)))

# 6. Revision pinning: downloading at the commit SHA works and dedups.
pinned = hf_hub_download(repo_id=REPO, filename="config.json", revision=COMMIT)
check("pinned_revision", sha256_of(pinned) == FILES["config.json"]["sha256"])

print("E2E_OK")
