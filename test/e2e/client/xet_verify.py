"""E2E Xet verification: drive Shpiel with unmodified huggingface_hub 1.x —
hf_xet enabled (the default!) — proving uploads flow through the Xet CAS
API and downloads reconstruct at chunk level.

Environment:
  HF_ENDPOINT  Shpiel base URL
  E2E_REPO     repo id to create and push to

Prints E2E_XET_OK as the last line on success.
"""

import hashlib
import json
import os
import subprocess
import sys

from huggingface_hub import HfApi, hf_hub_download

ENDPOINT = os.environ["HF_ENDPOINT"]
REPO = os.environ["E2E_REPO"]

api = HfApi(endpoint=ENDPOINT, token="hf_e2e_dummy_token")


def check(name, cond, detail=""):
    if not cond:
        print(f"FAIL {name}: {detail}", file=sys.stderr)
        sys.exit(1)
    print(f"ok {name}")


try:
    import hf_xet  # noqa: F401

    xet_importable = True
except ImportError:
    xet_importable = False
check("hf_xet_available", xet_importable and not os.environ.get("HF_HUB_DISABLE_XET"),
      "hf_xet must be importable and enabled for this test")

# --- Fixture: big-enough binary files that the xet pipeline chunks them ---
os.makedirs("/tmp/xet-upload", exist_ok=True)
# 9 MiB of structured-but-not-constant data: chunks well (many CDC chunks),
# and materialization into the OCI backend crosses ociclient's 8 MiB
# chunk-upload boundary — the size class that once 416'd against Zot.
weights = bytes((i * 31 + (i >> 8)) % 256 for i in range(9 << 20))
config = b'{"model_type":"xet-e2e","hidden_size":128}'
with open("/tmp/xet-upload/model.safetensors", "wb") as f:
    f.write(weights)
with open("/tmp/xet-upload/config.json", "wb") as f:
    f.write(config)

sha_weights = hashlib.sha256(weights).hexdigest()

# 1. Create repo and upload through the xet pipeline (no disable flag!).
api.create_repo(REPO, exist_ok=True)
info = api.upload_folder(repo_id=REPO, folder_path="/tmp/xet-upload", commit_message="xet e2e upload")
check("upload_folder.oid", bool(info.oid), info)

# 2. Metadata: the LFS entry carries the exact sha256 hf_xet computed.
mi = api.model_info(REPO)
by_name = {s.rfilename: s for s in mi.siblings}
check("siblings", set(by_name) == {"config.json", "model.safetensors"}, set(by_name))
check("lfs_sha256", by_name["model.safetensors"].lfs.sha256 == sha_weights,
      f"{by_name['model.safetensors'].lfs.sha256} != {sha_weights}")

# 3. Download WITHOUT xet (regular HTTP path): proves materialization put
# real bytes into the backend.
plain = subprocess.run(
    [
        sys.executable,
        "-c",
        (
            "from huggingface_hub import hf_hub_download;"
            f"p = hf_hub_download(repo_id={REPO!r}, filename='model.safetensors',"
            f" cache_dir='/tmp/plain-cache');"
            "print(p)"
        ),
    ],
    env={**os.environ, "HF_HUB_DISABLE_XET": "1"},
    capture_output=True,
    text=True,
)
check("plain_download.exit", plain.returncode == 0, plain.stderr[-800:])
plain_path = plain.stdout.strip().splitlines()[-1]
with open(plain_path, "rb") as f:
    check("plain_download.sha256", hashlib.sha256(f.read()).hexdigest() == sha_weights)

# 4. Download WITH xet: the resolve headers advertise X-Xet-Hash, and
# hf_xet reconstructs the file chunk-by-chunk through the CAS API.
local = hf_hub_download(repo_id=REPO, filename="model.safetensors", cache_dir="/tmp/xet-cache")
with open(local, "rb") as f:
    check("xet_download.sha256", hashlib.sha256(f.read()).hexdigest() == sha_weights)

# 5. Re-upload identical content: xorb dedup means was_inserted=false paths
# and a converged commit.
info2 = api.upload_folder(repo_id=REPO, folder_path="/tmp/xet-upload", commit_message="retry")
check("noop_reupload", info2.oid == info.oid, f"{info2.oid} != {info.oid}")

# 6. The hf CLI end to end, xet on. Locate the download in the cache dir
# rather than parsing stdout (its format changes between CLI versions).
import glob

cli = subprocess.run(
    ["hf", "download", REPO, "model.safetensors", "--cache-dir", "/tmp/cli-cache"],
    capture_output=True,
    text=True,
    env={**os.environ, "HF_TOKEN": "hf_e2e_dummy_token"},
)
check("hf_cli.exit", cli.returncode == 0, cli.stderr[-800:])
matches = glob.glob("/tmp/cli-cache/**/model.safetensors", recursive=True)
check("hf_cli.file_present", len(matches) > 0, cli.stdout[-300:])
with open(matches[0], "rb") as f:
    check("hf_cli.sha256", hashlib.sha256(f.read()).hexdigest() == sha_weights)

print("E2E_XET_OK")
