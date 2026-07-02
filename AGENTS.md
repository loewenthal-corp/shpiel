# Shpiel — agent guide

Shpiel is an HF-compatible model relay: it speaks the Hugging Face Hub API
on the front — read, write, and the Xet protocol (so `hf`,
`huggingface_hub` 0.x and 1.x, vLLM, SGLang, and TGI work unchanged with
`HF_ENDPOINT` set) — and stores models in pluggable backends on the back:
filesystems (HF-cache layout) and OCI registries (Zot/Harbor) today; S3
and upstream HF mirroring next. See [spec.md](spec.md) for the full
product spec and milestones (M0, M1, and M3/M4-core are done).

## Architecture in one pass

```diagram
client (hf / vLLM / push_to_hub)
   │  HF Hub API (read + write)
   ▼
internal/server     HTTP surface; routes parsed by internal/hfapi.ParseRoute
   ▼
internal/relay      backend-first reads; pull-through on miss (singleflight);
   │                commit application + LFS blob intake on writes
   ├── internal/backend/fsbackend    HF-cache-layout store (refs/manifests/blobs)
   ├── internal/backend/ocibackend   OCI artifacts in Zot/Harbor (modelpack or
   │      │                          mountable tar-layers; staged→promoted commits)
   │      └── internal/ociclient     minimal distribution-spec client (ranged
   │                                 reads, chunked streaming uploads)
   ├── internal/xet                  Xet protocol server: CAS API (xorb/shard
   │                                 ingest + reconstruction), format parsers,
   │                                 content-addressed store; ingested files
   │                                 materialize into the routed backend
   └── internal/upstream             huggingface.co client (pull-through source)
```

Storage invariant: blobs are keyed by content sha256 everywhere (the only
address OCI speaks); git-sha1 OIDs are metadata for ETags. All backends
accept manifests before blobs (staging) and link/promote as blobs arrive.

- `internal/hfapi` is the wire contract: JSON shapes, headers
  (`X-Repo-Commit`, `X-Linked-Etag`), error codes (`RepoNotFound`, ...).
  **Compatibility is the product** — never "improve" these.
- `internal/backend` is the driver interface (refs → manifests →
  content-addressed blobs). New backends implement it and register in
  `internal/app`.
- `internal/fakehub` is a hermetic huggingface.co simulator used by unit
  tests, the Tilt environment, and e2e. Tests never touch the internet.

## Building & checking

```sh
source bin/activate-hermit   # or direnv allow
task do                      # generate + format + lint + test + build
task test                    # just Go tests
task e2e                     # real huggingface_hub client in Docker vs shpiel
task dev                     # tilt up (needs a local k8s cluster)
```

CI runs `task do`, `task test:full` (race), `task e2e`, and fails on
uncommitted generated files.

## The testing story (read this before changing the API surface)

1. **Conformance suite** (`test/conformance`) is the executable spec of
   the HF API. The same read contract runs against every serving
   configuration: direct-seeded FS (cache hit), pull-through-from-fakehub
   (cache miss), write-then-read (the full write protocol pushes the
   fixture, then the read contract runs on what landed), and all of those
   again on the OCI backend in both formats. Every configuration must stay
   contract-identical. Point it at any live endpoint with
   `SHPIEL_CONFORMANCE_URL`.
2. **e2e** (`test/e2e`, `-tags e2e`) starts the real compiled binary and
   drives it with the real Python `huggingface_hub` + `hf` CLI (and real
   `hf_xet`) in Docker: pull-through downloads (`E2E_OK`), LFS uploads
   (`E2E_UPLOAD_OK`), pushes landing in a real Zot container, and Xet
   uploads + chunk-level downloads with no client flags (`E2E_XET_OK`).
3. Unit tests live next to their packages; `internal/relay` tests encode
   pull-through semantics (singleflight collapse, stale-ref revalidation,
   serve-stale-on-upstream-outage), `internal/xet` tests the binary
   formats round-trip.

When adding surface area: extend the conformance suite first, watch it
fail, then implement.

## Conventions

- Hermit pins the toolchain (`bin/`); never assume system Go.
- `Taskfile.yml` is the entry point for every workflow; add tasks there.
- Routing cannot use ServeMux patterns for HF-shaped URLs (the grammar
  overlaps); extend `hfapi.ParseRoute` and its table test instead.
- Config is one YAML (`config.example.yaml` documents it); flags > env >
  file > defaults. Secrets only via `*_env` indirection.
- Errors returned to clients must carry `X-Error-Code` — see
  `internal/server/errors.go`.
- Xet binary formats are ground-truthed against huggingface/xet-core, and
  shipping `hf_xet` wheels differ from that repo's main (footerless
  sequential shards, `/shards` without `/v1`, token info in `X-Xet-*`
  response headers). When a client rejects or sends something unexpected,
  set `SHPIEL_XET_DEBUG_DIR` to dump rejected shards and debug from real
  bytes — the real client is the oracle, not the docs.
