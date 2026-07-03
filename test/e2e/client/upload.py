"""E2E write-path verification: push a model to Shpiel with the real
huggingface_hub library (create_repo, upload_folder, delete_file) and the
hf CLI, then read everything back and verify bytes.

Environment:
  HF_ENDPOINT  Shpiel base URL
  E2E_REPO     repo id to create and push to

Prints E2E_UPLOAD_OK as the last line on success.
"""

import hashlib
import os
import subprocess
import sys

from huggingface_hub import HfApi, hf_hub_download

ENDPOINT = os.environ["HF_ENDPOINT"]
REPO = os.environ["E2E_REPO"]

# A dummy token: shpiel in auth.mode=none ignores it, but huggingface_hub
# refuses write actions without one client-side.
api = HfApi(endpoint=ENDPOINT, token="hf_e2e_dummy_token")


def check(name, cond, detail=""):
    if not cond:
        print(f"FAIL {name}: {detail}", file=sys.stderr)
        sys.exit(1)
    print(f"ok {name}")


# --- Fixture folder: small text (inline), big binary (LFS), nested file,
# and a model card. The weights are 9 MiB on purpose: LFS blobs at or
# above ociclient's 8 MiB chunk size take the chunked-commit path, which
# once 416'd against real Zot while 1 MiB uploads sailed through.
os.makedirs("/tmp/upload/vae", exist_ok=True)
config = b'{"model_type":"uploaded","hidden_size":64}'
weights = bytes((i * 7 + 3) % 256 for i in range(9 << 20))  # 9 MiB
nested = b'{"vae":true,"scale":0.5}'
readme = b"---\nlicense: apache-2.0\ntags:\n  - e2e\n---\n\n# e2e model\n"
with open("/tmp/upload/config.json", "wb") as f:
    f.write(config)
with open("/tmp/upload/model.safetensors", "wb") as f:
    f.write(weights)
with open("/tmp/upload/vae/config.json", "wb") as f:
    f.write(nested)
with open("/tmp/upload/README.md", "wb") as f:
    f.write(readme)

# 1. Repo creation (idempotent with exist_ok).
url = api.create_repo(REPO, exist_ok=True)
check("create_repo", REPO in str(url), url)
api.create_repo(REPO, exist_ok=True)
check("create_repo.exist_ok", True)

# 2. Full folder upload: preupload -> LFS batch -> PUT -> commit.
info = api.upload_folder(repo_id=REPO, folder_path="/tmp/upload", commit_message="e2e upload")
check("upload_folder.oid", bool(info.oid), info)

# 3. Metadata reflects the push.
mi = api.model_info(REPO)
names = {s.rfilename for s in mi.siblings}
check(
    "siblings",
    names == {"config.json", "model.safetensors", "vae/config.json", "README.md"},
    names,
)
check("model_info.sha", mi.sha == info.oid, f"{mi.sha} != {info.oid}")

# 4. Bytes round-trip through the read path.
for path, want in [
    ("config.json", config),
    ("model.safetensors", weights),
    ("vae/config.json", nested),
    ("README.md", readme),
]:
    local = hf_hub_download(repo_id=REPO, filename=path)
    with open(local, "rb") as f:
        got = f.read()
    check(f"roundtrip.{path}", got == want,
          f"{hashlib.sha256(got).hexdigest()} != {hashlib.sha256(want).hexdigest()}")

# 5. Re-uploading identical content is a no-op: same commit, no fork.
info2 = api.upload_folder(repo_id=REPO, folder_path="/tmp/upload", commit_message="retry")
check("noop_reupload", info2.oid == info.oid, f"{info2.oid} != {info.oid}")

# 6. Deletions commit and disappear from listings.
api.delete_file(path_in_repo="vae/config.json", repo_id=REPO, commit_message="rm vae config")
mi = api.model_info(REPO)
check("delete_file", "vae/config.json" not in {s.rfilename for s in mi.siblings})

# 7. Card pre-validation. upload_folder already called it implicitly for
# README.md (HfApi._validate_yaml); this exercises the rejection path,
# which no successful upload ever sends. The response must be JSON with
# warnings/errors lists — the client response.json()s it even on 400.
# get_session() is the client's own HTTP session (httpx on 1.x, requests
# on 0.x), so this stays import-compatible with both.
from huggingface_hub.utils import get_session  # noqa: E402

r = get_session().post(
    f"{ENDPOINT}/api/validate-yaml",
    json={"repoType": "model", "content": readme.decode()},
)
check(
    "validate_yaml.ok",
    r.status_code == 200 and r.json().get("errors") == [],
    f"{r.status_code}: {r.text[:200]}",
)
r = get_session().post(
    f"{ENDPOINT}/api/validate-yaml",
    json={"repoType": "model", "content": "---\nlicense: [unclosed\n---\n"},
)
check(
    "validate_yaml.rejects",
    r.status_code == 400 and r.json()["errors"][0]["message"] != "",
    f"{r.status_code}: {r.text[:200]}",
)

# 8. The hf CLI upload — the literal M1 exit criterion shape.
with open("/tmp/extra.safetensors", "wb") as f:
    f.write(bytes((i * 13 + 1) % 256 for i in range(64 << 10)))
cli = subprocess.run(
    ["hf", "upload", REPO, "/tmp/extra.safetensors", "extra.safetensors"],
    capture_output=True,
    text=True,
    env={**os.environ, "HF_TOKEN": "hf_e2e_dummy_token"},
)
check("hf_cli_upload", cli.returncode == 0, cli.stderr[-500:])
local = hf_hub_download(repo_id=REPO, filename="extra.safetensors")
with open("/tmp/extra.safetensors", "rb") as f:
    want = f.read()
with open(local, "rb") as f:
    check("hf_cli_roundtrip", f.read() == want)

print("E2E_UPLOAD_OK")
