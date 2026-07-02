# Product Spec: Shpiel — HF-Compatible Model Relay

**Status:** Draft v0.2
**Author:** Matanya Loewenthal
**Name:** `shpiel` — Yiddish for a story. Stories get passed around in different languages while keeping the same core truth; Shpiel passes models around in different protocols (HF, OCI, S3) while keeping the same core bytes. It also rhymes with the ecosystem: Spegel (Swedish, "mirror") reflects layers between peers; Shpiel retells the model to whoever asks, in whatever dialect they speak.

---

## 1. One-liner

A single Go binary that speaks the Hugging Face Hub API on the front (upload *and* download, including Xet) and writes to arbitrary backends on the back — OCI registries, object storage, filesystems, or upstream HF itself — so that every existing HF tool (`hf` CLI, `huggingface_hub`, `push_to_hub`, vLLM, SGLang, TGI) works unchanged by setting one environment variable:

```bash
export HF_ENDPOINT=https://shpiel.internal
```

## 2. Problem

The ML world has standardized on two incompatible planes:

- **Researcher plane:** Hugging Face is the de facto interface. Every training script ends in `push_to_hub()`. Every inference engine starts with `from_pretrained()` / `--model org/name`. Nobody wants to learn a new tool.
- **Infrastructure plane:** Clusters want weights as versioned, content-addressed, P2P-distributable artifacts — OCI registries (Zot, Harbor), image volumes, Spegel, Dragonfly. This is where cold-start time, egress cost, and reliability are actually won.

Today the bridge between these planes is ad hoc: shell scripts, MatrixHub-style self-hosted hubs (heavy, stateful, their own auth universe), or manual `hf download && modctl build && modctl push` pipelines. Every one of them breaks the researcher workflow or adds an ops burden.

**The specific pain that motivates this project:** autoscaled GPU fleets. With node auto-provisioning (Karpenter, GKE NAP, NeoCloud provisioners), the node *does not exist* until a workload needs it. Time-to-first-token is dominated by: provision node → pull 20 GB runtime image → pull 100–600 GB of weights from the public internet. Shpiel collapses the last step into a LAN-speed pull from in-cluster infrastructure, and — paired with Spegel — into a peer-to-peer pull from neighboring nodes.

## 3. Product principles

1. **Zero workflow breakage.** If a tool works against `huggingface.co`, it works against Shpiel. This includes auth, error semantics, and pagination quirks. Compatibility is the product.
2. **Boring to operate.** One static binary. One YAML file. No database requirement in the default configuration. Runs on a laptop, a VM, or Kubernetes identically.
3. **Backends are pluggable, the front is sacred.** The HF API surface is the stable contract; backends are drivers behind an interface.
4. **Content-addressed all the way down.** Dedup at chunk level (Xet) or layer level (OCI) — never store the same bytes twice, never transfer them twice.
5. **Built to make Spegel look good.** First-class output is an OCI artifact in an in-cluster registry, mountable via K8s image volumes and distributable by Spegel. This is the flagship deployment story, not an afterthought.

## 4. Target users & use cases

| User | Use case |
|---|---|
| ML platform team | In-cluster HF endpoint; researchers push once, weights land in Zot/Harbor as ModelPack OCI artifacts + optionally mirror to real HF |
| NeoCloud / GPU provisioner | Pre-warm layer: every new node's vLLM/SGLang pull is served at LAN/P2P speed instead of WAN |
| Air-gapped / regulated org (SNF-style compliance) | Ferry models across the boundary once; serve `HF_ENDPOINT`-compatible reads internally with audit logging |
| Individual dev | Local shim: `shpiel serve --local`; `push_to_hub` lands weights in a kind cluster registry in seconds |
| CI systems | Deterministic model provenance: pin revision → immutable OCI digest, GitOps-able |

## 5. Functional requirements

### 5.1 HF API surface (frontend)

**Read path (v1, table stakes):**
- `GET /api/models/{repo}` and `/api/models/{repo}/revision/{rev}` — repo metadata, siblings list, sha
- `GET /{repo}/resolve/{rev}/{filename}` — file resolution with correct `X-Repo-Commit`, `X-Linked-Etag`, `X-Linked-Size`, redirect/streaming semantics, Range support (critical: `hf_hub_download`, vLLM, and `safetensors` lazy loading all depend on Range)
- `GET /api/models/{repo}/tree/{rev}` — file listing with LFS metadata
- `GET /api/whoami-v2` — token validation (see auth)
- Revision semantics: branch names, tags, commit SHAs; `main` default
- Pull-through mode: on miss, fetch from upstream `huggingface.co` (with configured org token), persist to backend, serve. This is the MatrixHub-killer feature.

**Write path (v1):**
- Commit API: `POST /api/models/{repo}/commit/{rev}` (NDJSON commit payload)
- Preupload: `POST /api/models/{repo}/preupload/{rev}` — decide per-file: inline vs LFS vs Xet
- LFS batch API + S3-style multipart upload endpoints (what `huggingface_hub` falls back to when Xet is unavailable)
- Repo management: create/delete repo, branch/tag create, move
- `huggingface_hub` gracefully degrades to the LFS path when the endpoint doesn't advertise Xet — **v1 ships LFS-complete, Xet-advertising-off**, which already makes every client work.

**Xet (v1.x, the differentiator):**
- The Xet protocol is publicly specified (chunking via gearhash CDC, chunk/xorb/shard hashing, xorb format, CAS HTTP API, reconstruction API, token issuance). Clients are open source (`xet-core`, `hf_xet`); the server is not — Shpiel becomes the first OSS Xet-speaking server.
- Implement: token endpoint (`/api/{type}s/{repo}/xet-{read,write}-token/{rev}`), xorb upload, shard upload, global dedup query, reconstruction API with fetch-info URLs pointing at Shpiel itself (or presigned backend URLs).
- Payoff: chunk-level dedup across fine-tunes (a fine-tune that shares 95% of chunks costs ~5% storage/transfer), resumable multiplexed transfers, and full-speed `hf_xet` clients.
- Explicit scope gate: Xet write ships behind a feature flag; Xet read (reconstruction) may ship earlier since it's simpler and accelerates cluster-internal downloads.

**Out of scope for frontend:** Spaces, datasets viewer, discussions/PRs, inference API. Datasets repos (`/api/datasets/...`) are v1.x — same mechanics, second priority.

### 5.2 Backends

All backends implement a common interface (`Put/Get/Stat/List/Delete` at blob granularity + repo metadata store). Configurable per-route with fan-out.

| Backend | Notes |
|---|---|
| **OCI registry** (flagship) | Writes ModelPack-spec artifacts (modctl-compatible manifest + config), with a config option for **standard `tar` layer media types** so K8s image volumes mount them natively (avoids the custom-media-type empty-mount trap). One safetensors file ↔ one layer for maximal Spegel/dedup granularity. Targets: Zot, Harbor, GHCR, GAR, ECR. |
| **Filesystem / NFS / PVC** | HF cache-compatible layout (`models--org--name/snapshots/{sha}`) so a shared PVC is directly consumable by `from_pretrained` with `HF_HUB_OFFLINE=1` |
| **S3 / GCS / Azure Blob** | Blob store keyed by content hash; doubles as the Xet xorb store |
| **Upstream HF** | As a *mirror target* (push-through: researcher pushes to Shpiel, Shpiel pushes to real HF with org credentials) and as a *pull-through source* |

**Fan-out / replication:** a push can be routed to N backends (e.g., `zot-cluster-a` + `zot-cluster-b` + `huggingface.co`). Per-target async with retry queue; commit is acknowledged when the *primary* target durably has it, replicas reconcile in background. Replication status queryable via admin API.

**Routing rules:** glob-based, e.g. `exigence/*` → internal-only; `public/*` → internal + mirror to HF.

### 5.3 Auth & credentials (no workflow breakage)

- **Token passthrough:** accepts `Authorization: Bearer hf_...`. In passthrough mode, validates against upstream HF (`whoami-v2`) with short-TTL caching — researchers' existing tokens Just Work.
- **Local tokens:** Shpiel can mint its own tokens (`shpiel token create --user alice --scope write:exigence/*`) for air-gapped/no-upstream deployments. Fine-grained scopes mirroring HF's model (read/write per-namespace).
- **Static/OIDC:** map OIDC identities (WorkOS/Google/GitHub) → namespaces for orgs that want SSO. v1.x.
- **Backend credentials:** configured server-side (registry creds, S3 keys, upstream HF org token) — researchers never see or handle them. Support for K8s Secrets, env vars, and file mounts; GCP/AWS ambient credentials (workload identity) preferred where available.
- **Admin API:** `/admin/v1/*` — token CRUD, replication status, cache eviction, backend health. Separate listener/port, separately authenticated (mTLS or admin token), off by default.

### 5.4 Deployment modes

1. **Standalone binary.** `shpiel serve --config config.yaml`. Single static Go binary (CGO_ENABLED=0), amd64/arm64, ~<30 MB. Also `shpiel serve --local` for zero-config laptop mode (localhost bind, filesystem backend in `~/.shpiel`).
2. **Container image.** Distroless, published to GHCR.
3. **Helm chart.** Published as OCI chart (`oci://ghcr.io/.../charts/shpiel`). Values map 1:1 to `config.yaml` (chart renders the same YAML into a ConfigMap — one mental model, no translation layer). Includes ServiceMonitor, HPA (read-path), PDB, optional Ingress/Gateway.
4. **Kustomize.** Plain manifests + kustomize overlays maintained in-repo for the no-Helm crowd, kept in lockstep with the chart via CI.

### 5.5 Configuration

- **[alecthomas/kong](https://github.com/alecthomas/kong)**: one struct defines the entire CLI — flags, env bindings (`SHPIEL_*`), and defaults — with `kong-yaml` as the config resolver, giving precedence flags > env > `config.yaml` > struct defaults without Viper's global-state footguns or key-case surprises. `shpiel config validate` subcommand.
- Hot-reload for routing rules and token maps (SIGHUP / fsnotify); listener/backend changes require restart.

```yaml
# config.yaml (illustrative)
listen:
  api: ":8080"
  admin: ":8081"      # disabled unless set
  metrics: ":9090"

limits:
  max_concurrent_uploads: 64
  max_concurrent_downloads: 512
  per_conn_buffer_mb: 8

upstream:
  huggingface:
    endpoint: https://huggingface.co
    token_env: HF_ORG_TOKEN
    pull_through: true

backends:
  zot:
    type: oci
    url: https://zot.internal:5000
    format: modelpack          # or: tar-layers (image-volume-safe)
    layer_per_file: true
    auth: { username_env: ZOT_USER, password_env: ZOT_PASS }
  archive:
    type: s3
    bucket: models-archive
    region: us-central1

routes:
  - match: "exigence/*"
    primary: zot
    replicas: [archive]
  - match: "*"
    primary: zot
    replicas: [huggingface]    # push-through to real HF

auth:
  mode: passthrough            # passthrough | local | oidc
  cache_ttl: 5m

xet:
  enabled: false               # v1.x
```

### 5.6 Observability

- **Prometheus** `/metrics`: `shpiel_http_requests_total{route,method,code}`, request duration histograms, `shpiel_upload_bytes_total{backend}`, `shpiel_download_bytes_total{source=cache|upstream|backend}`, cache hit ratio, `shpiel_pullthrough_fetches_total`, replication queue depth + lag, per-backend error counters, in-flight connections, Xet chunk-dedup ratio (later).
- **Health:** `/healthz` (liveness), `/readyz` (backend reachability).
- **Logs:** structured `slog`, JSON default; audit log stream for all writes and admin actions (who/what/when/digest) — table stakes for the regulated-org story.
- **Traces:** OpenTelemetry (OTLP export), spans per commit → per-file → per-backend write.
- **Debug UI:** minimal read-only web page (à la Spegel's debug interface): recent transfers, throughput, replication status. Not a hub UI — deliberately not competing with HF's web experience.

## 6. Scaling model (answering the "does this need to scale horizontally?" question)

**Verdict: vertical-first by design; read path horizontally scalable from day one; write path single-writer in v1 with a documented path to N.**

Rationale:

- This is bandwidth-bound, not CPU-bound. A single Go process on a node with a 25–100 Gbps NIC will saturate the network long before goroutines are the limit — thousands of concurrent connections is a non-problem for `net/http`. Concurrency limits are config knobs (`limits:` above), which covers "handle multiple connections off of one, configurable."
- **Read path (downloads, pull-through) is stateless** → N replicas behind a Service work immediately. HPA on connection count / NIC throughput. This is the path that matters for the pre-warm use case (100 nodes pulling simultaneously), and it scales trivially — and in the flagship deployment it barely matters anyway, because Spegel absorbs the fan-out (Shpiel/Zot serve each layer roughly once; peers serve the rest).
- **Write path has session state:** a multi-file commit involves preupload → N parallel file uploads → commit, and multipart uploads have part-tracking state. v1 keeps this in-memory + spooled to local disk, which means **one writer replica or sticky sessions** (cookie/consistent-hash on repo). This is fine: write QPS is researcher-scale (tens/day), not machine-scale.
- **v2 horizontal writes,** if ever needed: move upload-session state to Redis/Postgres and spool parts directly to the object-store backend (S3 multipart natively supports distributed part upload). Design the session store behind an interface now; implement `memory` only in v1.
- **No consensus, no leader election, no database requirement in v1.** Repo metadata lives in the backend itself (OCI manifests / a small index object in S3 / files on disk), keeping Shpiel restart-safe and effectively stateless between requests.

## 7. The Spegel story (flagship deployment)

Dedicated docs page + reference architecture, co-marketed:

```
researcher: hf upload exigence/gemma-4-31b-ft ...   (HF_ENDPOINT=shpiel)
     │
     ▼
  Shpiel ──▶ Zot (in-cluster OCI, ModelPack/tar layers)
                 │
   K8s image volume: volumes: [{image: {reference: zot/models/...}}]
                 │
             containerd ◀──── Spegel DHT ────▶ peer nodes' containerd
```

- First node pulls each layer from Zot once; every subsequent node pulls from peers. Weights are just layers; Spegel already knows how to do this. Shpiel's job is getting weights *into* that plane without anyone learning a new tool.
- **Pre-warm flow for autoscaled fleets:** node comes up (Karpenter/GKE NAP) → workload references the weights image volume → containerd asks Spegel → layers stream from N peers in parallel → vLLM/SGLang starts. Optional `shpiel warm zot/models/x:v1 --nodes selector` helper (or a tiny DaemonSet hook) to pre-seed before the workload lands.
- Commit to contributing benchmarks upstream: publish a repeatable Spegel + image-volumes weights benchmark (cold node → tokens flowing) as part of the repo. "Make Spegel awesome" is an explicit project goal — Shpiel is the on-ramp, Spegel is the freeway.
- Same story works with Dragonfly for orgs already running it (Shpiel → registry → Dragonfly P2P); documented but not the flagship.

## 8. Non-goals

- Not a model hub UI (no browsing/cards/discussions) — HF remains the social layer.
- Not an inference server, gateway, or router.
- Not a training artifact store (checkpoints-during-training may work via the FS backend, but it's not designed for high-frequency checkpoint churn in v1).
- Not GPU-to-GPU transfer (that's ModelExpress's layer; Shpiel feeds the seed node).
- No Git protocol support (`git clone` of model repos) — resolve/tree APIs cover the real clients.

## 9. Milestones

| Milestone | Scope | Exit criteria |
|---|---|---|
| **M0 — Skeleton** (2–3 wk) | Binary, kong CLI/config, read path + pull-through, FS backend, metrics, healthz | `HF_ENDPOINT=shpiel` + `hf download` + vLLM `--model` work against pull-through cache |
| **M1 — Write path** (3–4 wk) | Commit/preupload/LFS-multipart, token passthrough, OCI backend (ModelPack + tar-layer modes) | `push_to_hub()` and `hf upload` land a mountable OCI artifact in Zot; image-volume mount verified on GKE |
| **M2 — Ops-ready** (2–3 wk) | Helm chart + Kustomize, fan-out replication + retry queue, admin API, audit log, dashboards, local mode polish | Chart published; Spegel reference architecture doc + benchmark published |
| **M3 — Xet read** | Reconstruction API + xorb store on S3 backend | `hf_xet` clients download at chunk level from Shpiel |
| **M4 — Xet write** | CAS ingest (xorb/shard upload, dedup query, token issuance) | Chunk-dedup ratio metric shows real savings across fine-tune pushes |

## 10. Open questions

1. Xet fetch-info URLs: serve bytes through Shpiel vs. presign directly to S3 — bandwidth vs. simplicity tradeoff, likely configurable.
2. Should pull-through also *transform* (HF repo → OCI artifact on first pull) or only cache in HF layout? Leaning yes-transform — it makes the Spegel path automatic even for public models nobody explicitly pushed.
3. Dataset repos priority — several likely users care more about datasets than models.
4. License: Apache-2.0 (assumed; matches ecosystem).
5. Garbage collection / retention policy for pull-through cache (LRU by size? pinned tags?).