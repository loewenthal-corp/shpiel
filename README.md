# Shpiel

**Push Hugging Face models straight into your OCI registry.** Shpiel
speaks the Hugging Face Hub API — read, write, and Xet — so
`push_to_hub()` lands weights as versioned, content-addressed OCI
artifacts in the registry your cluster already runs, ready for image
volumes and P2P distribution. (Filesystem and S3-compatible object
storage backends ship too.) Every existing HF tool works unchanged by
setting one environment variable:

```bash
export HF_ENDPOINT=https://shpiel.internal
```

`hf download`, `push_to_hub()`, vLLM `--model org/name`, SGLang, TGI — no
new tools, no new auth universe, no workflow breakage.

## Why

The ML world standardized on two incompatible planes. Researchers live on
the Hugging Face API: every training script ends in `push_to_hub()`, every
inference engine starts with `from_pretrained()`. Clusters want weights as
versioned, content-addressed, P2P-distributable artifacts — that's where
cold-start time, egress cost, and reliability are actually won.

Today the bridge between those planes is shell scripts, or a heavyweight
self-hosted hub with its own database and auth universe. Shpiel is the
bridge as one boring binary: no database, one YAML file, runs identically
on a laptop and in Kubernetes.

The killer scenario is autoscaled GPU fleets: with node auto-provisioning,
time-to-first-token is dominated by pulling hundreds of gigabytes of
weights from the public internet onto a node that didn't exist a minute
ago. Shpiel turns that into a LAN-speed pull from in-cluster
infrastructure — and, paired with [Spegel](https://spegel.dev), into a
peer-to-peer pull from neighboring nodes. See
[the Spegel deployment guide](docs/spegel.md).

## The pieces

```diagram
researchers ── HF API (read / write / Xet) ──▶ Shpiel ──▶ backends
inference engines ◀── HF API (reads, ranges) ──┘   │        ├─ OCI registry (Zot, Harbor)
                                                   │        ├─ filesystem / NFS / PVC
                                                   │        └─ S3 / GCS / MinIO / R2
                                     huggingface.co (pull-through / mirror)
```

- **The HF front is the whole product.** Repo info, file resolution with
  byte ranges, tree listings, commits, preupload, git-LFS batch uploads,
  token passthrough — matching the Hub's headers, error codes, and
  pagination quirks, because compatibility is only useful when it's exact.
- **Xet, server-side.** `huggingface_hub` 1.x uploads through the Xet
  protocol with no LFS fallback; Shpiel implements the CAS API (the first
  open-source server that does), so current clients push and pull
  chunk-level with zero client-side flags.
- **Pull-through caching.** On a miss, Shpiel fetches from huggingface.co,
  persists to your backend, and serves — with request collapsing so a
  hundred nodes asking for the same model cost one upstream download.
- **OCI backend.** Models land as OCI artifacts: one repository per model,
  one manifest per commit (tagged by SHA), refs as human tags, one layer
  per file. The `tar-layers` format is a standard mountable image —
  Kubernetes image volumes and Spegel work out of the box.
- **Filesystem backend.** Byte-compatible with the `huggingface_hub`
  cache: mount the volume, set `HF_HUB_OFFLINE=1`, and `from_pretrained`
  reads it directly.
- **S3 backend.** Content-addressed blobs in any S3-compatible bucket —
  AWS S3, GCS in interop mode, MinIO, Ceph, R2 — as a primary store or as
  the archive replica behind an OCI primary. Hand-rolled SigV4, no SDK;
  credentials via env vars or ambient IRSA web identity (no static keys
  in the pod). The same bucket can double as the Xet xorb store
  (`xet.store_backend`), so chunk-level storage needs no local disk.
- **Fan-out replication.** Routes can declare replicas (a second registry,
  another cluster); pushes replicate asynchronously through a disk-spooled
  retry queue. No database, restart-safe.
- **Ops built in.** Prometheus metrics with a
  [ready-made Grafana dashboard](dashboards/shpiel-grafana.json), an
  append-only audit stream, an authenticated admin API, structured logs,
  health/readiness probes.

## Install

```bash
# Container image
docker run -p 8080:8080 ghcr.io/loewenthal-corp/shpiel:latest \
  serve --local --listen-api :8080

# Helm chart
helm install shpiel oci://ghcr.io/loewenthal-corp/charts/shpiel

# Binaries (linux/darwin, amd64/arm64) on every release
# https://github.com/loewenthal-corp/shpiel/releases

# From source
go install github.com/loewenthal-corp/shpiel/cmd/shpiel@latest
```

## Quickstart

```bash
# Laptop mode: localhost bind, filesystem store in ~/.shpiel,
# pull-through from huggingface.co, Xet on.
shpiel serve --local

# Point any HF tool at it:
export HF_ENDPOINT=http://127.0.0.1:8080
hf download Qwen/Qwen3-0.6B        # first pull: cached through Shpiel
hf download Qwen/Qwen3-0.6B        # second pull: served locally
hf upload my-org/my-model ./model  # pushes work too
```

Beyond laptop mode, everything is one YAML file —
[config.example.yaml](config.example.yaml) documents every knob:

```bash
shpiel config validate config.yaml
shpiel serve --config config.yaml
```

The Helm chart uses the same mental model: its `config` value *is*
config.yaml, rendered verbatim into a ConfigMap.

## Compatibility

| Client | Reads | Writes |
|---|---|---|
| `huggingface_hub` / `hf` CLI 1.x | ✅ (HTTP or chunk-level Xet) | ✅ via Xet (enable `xet` in config) |
| `huggingface_hub` / `hf` CLI 0.x | ✅ | ✅ via git-LFS |
| vLLM, SGLang, TGI, `from_pretrained` | ✅ incl. Range/lazy loading | — |
| `HF_HUB_OFFLINE=1` on a shared volume | ✅ (fs backend) | — |

The compatibility surface is enforced by an executable conformance suite
that runs against every serving configuration, and by end-to-end tests
that drive a real Python `huggingface_hub`/`hf_xet` client against the
real binary. If it regresses, CI fails.

## Status

**v0.1.0 is out.** The read path, write path, Xet server, OCI,
filesystem, and S3 backends, pull-through, replication, and the ops
surface described above are all shipped and covered by conformance + e2e
tests.

On the roadmap: global chunk-level dedup queries, dataset repos, mintable
local tokens and OIDC for air-gapped auth, and published Spegel benchmark
numbers. The [spec](spec.md) tracks the details.

## Shpiel vs. the alternatives

**MatrixHub-style self-hosted hubs.** Full hub replacements: a database,
a web UI, their own user/org/auth universe to administer. Shpiel
deliberately isn't a hub — huggingface.co stays the social layer (cards,
browsing, discussions) while Shpiel handles bytes. No database, no UI,
stateless between requests, and your weights live in a standard registry
instead of an app-specific store.

**Dragonfly.** P2P distribution for registries — it accelerates pulls but
doesn't get models *into* a registry, so you still need the
HF-to-OCI bridge. Shpiel is that bridge, and the two compose: point
Shpiel's OCI backend at a Dragonfly-fronted registry and pulls fan out
P2P. (Spegel plays the same role with zero extra infrastructure, which is
why it's the flagship pairing — see [docs/spegel.md](docs/spegel.md).)

**ModelExpress.** GPU-to-GPU weight transfer between running processes —
a layer *above* storage. It needs a seed node to have the weights in the
first place; Shpiel is how the seed node gets them at LAN speed.

**Shell scripts** (`hf download && modctl build && modctl push`). Work
until they meet a moving branch, a resume, a fine-tune push, or a
researcher who wasn't told about them. Shpiel keeps the researcher
workflow byte-identical and makes the pipeline a server instead of a
cron job.

## Contributing

Development setup, the Tilt environment, testing philosophy, and the
release process live in [DEVELOPMENT.md](DEVELOPMENT.md). Agent-oriented
notes live in [AGENTS.md](AGENTS.md).

## License

Apache-2.0
