# Shpiel

**An HF-compatible model relay.** Shpiel speaks the Hugging Face Hub API on
the front and writes to arbitrary backends on the back — filesystems today,
OCI registries (Zot, Harbor), object storage, and upstream HF mirroring
next — so every existing HF tool works unchanged by setting one environment
variable:

```bash
export HF_ENDPOINT=https://shpiel.internal
```

`hf download`, `push_to_hub()`, vLLM `--model org/name`, SGLang, TGI: no
new tools, no new auth universe, no workflow breakage. *Shpiel* is Yiddish
for a story — stories get retold in different languages while keeping the
same core truth; Shpiel retells models in different protocols (HF, OCI, S3)
while keeping the same core bytes.

## Why

The ML world standardized on two incompatible planes: researchers live on
the Hugging Face API, clusters want weights as content-addressed,
P2P-distributable artifacts. The bridge is ad hoc shell scripts or heavy
self-hosted hubs. Shpiel is the bridge as one boring binary: a pull-through
cache in front of huggingface.co that lands weights in in-cluster
infrastructure, where [Spegel](https://spegel.dev) and Kubernetes image
volumes can fan them out at LAN speed. See [spec.md](spec.md) for the whole
story.

## Quickstart

```bash
# Laptop mode: localhost bind, filesystem store in ~/.shpiel,
# pull-through from huggingface.co.
shpiel serve --local

# Point any HF tool at it:
HF_ENDPOINT=http://127.0.0.1:8080 hf download Qwen/Qwen3-0.6B
# Second download (any machine sharing the cache): served locally.
```

With a config file (see [config.example.yaml](config.example.yaml)):

```bash
shpiel config validate config.yaml
shpiel serve --config config.yaml
```

## Status

Early. M0 (read path) is functional and hard-tested: repo info / tree /
resolve with Range support, pull-through caching with singleflight
collapse, token passthrough, Prometheus metrics, structured logs. The
on-disk store is byte-compatible with the `huggingface_hub` cache, so a
volume Shpiel fills is directly consumable with `HF_HUB_OFFLINE=1`. Write
path (commit/preupload/LFS), OCI + S3 backends, and Xet are next — see
[spec.md](spec.md) §9 for milestones.

## Development

Tooling is pinned with [Hermit](https://cashapp.github.io/hermit/); tasks
run through [Task](https://taskfile.dev):

```bash
source bin/activate-hermit   # or: direnv allow
task do                      # generate + format + lint + test + build
task e2e                     # real hf client (Docker) against a real shpiel
task dev                     # tilt up: hermetic k8s dev env (shpiel + fakehub + zot)
```

The test suite is the spec: `test/conformance` encodes the HF API contract
and runs against both the cache-hit and pull-through paths; `test/e2e`
proves an unmodified `huggingface_hub`/`hf` CLI works end to end. See
[AGENTS.md](AGENTS.md) for the layout and testing story.

## License

Apache-2.0
