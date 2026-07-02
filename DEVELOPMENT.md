# Developing Shpiel

Everything you need to build, test, and release Shpiel. For the product
spec see [spec.md](spec.md); for agent-oriented conventions see
[AGENTS.md](AGENTS.md).

## Prerequisites

- **[Hermit](https://cashapp.github.io/hermit/)** pins the entire
  toolchain (Go, task, tilt, helm, golangci-lint, ...) in `bin/`. Nothing
  else to install:

  ```bash
  source bin/activate-hermit     # or `direnv allow` — .envrc does this
  ```

- **Docker** for the e2e suite and Tilt image builds (OrbStack, Docker
  Desktop, or any daemon).
- **A local Kubernetes cluster** for the Tilt environment (OrbStack's
  built-in cluster, kind, or Docker Desktop).

## Everyday commands

```bash
task do                # the full gate: generate → format → lint → test → build
task test              # Go tests only
task test:full         # with race detection and coverage (what CI runs)
task e2e               # real huggingface_hub client in Docker vs the real binary
task dev               # tilt up
task build             # dist/shpiel
task release:snapshot  # dry-run the GoReleaser build (all platforms, no publish)
```

CI runs `task do`, `task test:full`, `task e2e`, and fails if generated
files are uncommitted — if `task do` is green locally, CI will agree.

## Repository layout

```
cmd/shpiel/                  kong CLI (serve, config validate, version)
cmd/fakehub/                 hermetic huggingface.co simulator (dev/test only)
internal/hfapi/              the HF wire contract: types, headers, URL grammar
internal/server/             HTTP surface, auth, admin API
internal/relay/              orchestration: pull-through, commits, fan-out
internal/backend/            storage driver interface
internal/backend/fsbackend/  HF-cache-layout filesystem store
internal/backend/ocibackend/ OCI artifacts (modelpack / tar-layers)
internal/ociclient/          minimal OCI distribution client
internal/xet/                Xet protocol server: CAS API, formats, store
internal/replication/        disk-spooled async fan-out queue
internal/upstream/           huggingface.co client (pull-through source)
internal/{audit,metrics,config,app,fakehub,buildinfo}
test/conformance/            executable spec of the HF API
test/e2e/                    real-client end-to-end tests (-tags e2e)
charts/shpiel/               Helm chart (values.config IS config.yaml)
kustomize/, containers/      dev-cluster manifests and Dockerfiles
```

## The testing philosophy

Compatibility is the product, so the tests are the spec:

1. **Conformance** (`test/conformance`) encodes the HF API contract —
   headers, byte ranges, error codes, pagination — and runs it against
   every serving configuration: direct-seeded filesystem, pull-through
   from fakehub, write-then-read (push the fixture through the full write
   protocol, then run the read contract on what landed), and all of the
   above on the OCI backend in both formats. Every configuration must be
   contract-identical. Point the suite at any live deployment with
   `SHPIEL_CONFORMANCE_URL=https://... go test ./test/conformance`.
2. **e2e** (`test/e2e`, behind `-tags e2e`) compiles the real binary and
   drives it with the real Python `huggingface_hub` + `hf` CLI + `hf_xet`
   in Docker: pull-through downloads, LFS uploads, pushes landing in a
   real Zot container, and Xet uploads + chunk-level downloads with no
   client flags. This is the "works with unmodified tools" proof.
3. **Unit tests** live next to their packages and pin the tricky
   semantics: singleflight collapse, stale-ref revalidation, replication
   retry/restart/ordering, Xet binary format round-trips.

Adding API surface? Extend the conformance suite first, watch it fail,
then implement.

## The Tilt environment

`task dev` (or `tilt up`) brings up a fully hermetic cluster environment —
nothing touches the public internet:

| Resource | What it is | Port |
|---|---|---|
| `shpiel` | the relay, backed by in-cluster Zot (tar-layers) with Xet on | 8080 (api), 9090 (metrics) |
| `zot` | OCI registry, the backend | 5000 |
| `fakehub` | huggingface.co simulator seeded with fixture models | 8081 |
| `hf-smoke` | a Job running real `hf download` against shpiel | — |
| `go-test` / `go-lint` | run on every file save | — |

The smoke job re-runs from the Tilt UI; green means an unmodified HF
client pulled a model through shpiel → fakehub → Zot. Try it yourself:

```bash
HF_ENDPOINT=http://localhost:8080 hf download fixtures/tiny-model
curl -s localhost:8080/api/models/fixtures/tiny-model | jq .
```

Config lives in [kustomize/shpiel/dev/config.yaml](kustomize/shpiel/dev/config.yaml);
edit and Tilt redeploys.

## e2e details

`task e2e` needs a Docker daemon (`orb start` if the socket is missing).
The suite builds a small Python client image once (cached afterwards) and
runs each flow against a compiled binary on random ports, with fakehub as
the hermetic upstream. `SHPIEL_E2E_REQUIRE=1` (set in CI) turns
"Docker unavailable" from a skip into a failure.

Debugging Xet protocol issues: set `SHPIEL_XET_DEBUG_DIR=/some/dir` and
Shpiel dumps rejected shards there — debug from real client bytes, since
shipping `hf_xet` wheels differ from xet-core's main branch in places.

## Releasing

Releases are fully automated; the only human step is merging the release
PR.

1. Land conventional commits on `main` (`feat:`, `fix:`, ...).
2. release-please maintains a release PR that accumulates the changelog
   and bumps versions everywhere (`internal/buildinfo`, the Helm chart)
   via `x-release-please-version` markers.
3. Merging the PR tags `vX.Y.Z` and creates the GitHub release, which
   fans out automatically:
   - **Images**: multi-arch (linux/amd64 + arm64) to
     `ghcr.io/loewenthal-corp/shpiel` (`X.Y.Z`, `X.Y`, `latest`)
   - **Chart**: `oci://ghcr.io/loewenthal-corp/charts/shpiel`
   - **Binaries**: GoReleaser attaches linux/darwin × amd64/arm64
     archives + checksums to the release

Gotcha worth knowing: `charts/` is excluded from yamlfmt because
release-please rewrites `Chart.yaml` in its own style — don't hand-format
that tree.

## Conventions

- `Taskfile.yml` is the entry point for every workflow; add new ones there.
- Config is one YAML; flags > env > file > defaults. Secrets only ever
  enter via `*_env` indirection — config files stay committable.
- HF-shaped URLs can't use ServeMux patterns (the grammar overlaps);
  routing goes through `hfapi.ParseRoute` and its table test.
- Client-facing errors carry `X-Error-Code` — see
  `internal/server/errors.go`.
- Storage invariant: blobs are keyed by content sha256 everywhere;
  git-sha1 OIDs are ETag metadata only.
