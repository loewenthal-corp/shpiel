# The Spegel deployment: weights at LAN speed

This is Shpiel's flagship deployment shape (spec §7): researchers keep
pushing with `push_to_hub()`, and autoscaled GPU nodes pull weights
peer-to-peer instead of from the public internet.

```diagram
researcher: hf upload exigence/gemma-ft ...        (HF_ENDPOINT=shpiel)
     │
     ▼
  Shpiel ──▶ Zot (in-cluster OCI registry, tar-layer artifacts)
                 │
   K8s image volume: volumes: [{image: {reference: zot.../models/exigence/gemma-ft:main}}]
                 │
             containerd ◀──── Spegel DHT ────▶ peer nodes' containerd
```

The mechanics: Shpiel's OCI backend in `tar-layers` format writes each
model file as one standard OCI tar layer with a real image config, so the
artifact is mountable by Kubernetes [image volumes] and — because weights
are then *just layers* — distributable by [Spegel] with zero extra
machinery. Zot serves each layer roughly once; every subsequent node pulls
from its peers.

[image volumes]: https://kubernetes.io/docs/tasks/configure-pod-container/image-volumes/
[Spegel]: https://spegel.dev

## Shpiel configuration

```yaml
# config.yaml (or the `config` value of the Helm chart)
upstream:
  huggingface:
    endpoint: https://huggingface.co
    token_env: HF_ORG_TOKEN
    pull_through: true          # public models land in Zot on first pull

backends:
  zot:
    type: oci
    url: http://zot.registry.svc.cluster.local:5000
    format: tar-layers          # the image-volume-mountable format

routes:
  - match: "*"
    primary: zot

xet:
  enabled: true                 # hub 1.x pushes work with no client flags
  data_dir: /var/lib/shpiel-xet
```

Researchers set one variable and change nothing else:

```bash
export HF_ENDPOINT=http://shpiel.registry.svc.cluster.local:8080
```

## Consuming weights as image volumes

Every commit is tagged by SHA and every ref by name, so pods pin either:

```yaml
apiVersion: v1
kind: Pod
spec:
  containers:
    - name: vllm
      image: vllm/vllm-openai:latest
      args: ["--model", "/models"]
      volumeMounts:
        - name: weights
          mountPath: /models
          readOnly: true
  volumes:
    - name: weights
      image:
        reference: zot.registry.svc.cluster.local:5000/models/exigence/gemma-ft:main
        pullPolicy: IfNotPresent
```

GitOps-able provenance: pin the commit-SHA tag instead of `main` and the
mounted weights are immutable.

## Pre-warm flow for autoscaled fleets

1. Karpenter / GKE NAP provisions a node for a pending GPU workload.
2. The workload references the weights image volume.
3. containerd asks for the layers; Spegel resolves them from peer nodes
   already holding the model.
4. Layers stream from N peers in parallel; vLLM/SGLang starts without ever
   touching the WAN.

To seed the first copy before a fleet scales up, run a one-shot pull on
any existing node (a DaemonSet hook or a `crictl pull` Job works):

```bash
crictl pull zot.registry.svc.cluster.local:5000/models/exigence/gemma-ft:main
```

## Benchmark: cold node → weights mounted

`scripts/benchmark/spegel-bench.sh` measures the number that matters —
time from "empty containerd" to "weights fully pulled" — in three
configurations:

1. **WAN baseline**: pull from huggingface.co (what you have today).
2. **Zot direct**: pull from in-cluster Zot (Shpiel's floor).
3. **Spegel P2P**: pull with Spegel active and ≥1 peer warm (the ceiling).

Run it from any node (or debug pod) with `crictl` access:

```bash
./scripts/benchmark/spegel-bench.sh zot.registry.svc.cluster.local:5000/models/exigence/gemma-ft:main
```

It prints per-run wall-clock timings and layer counts; run it on a fresh
node for the cold numbers and re-run after `crictl rmi` for repeatability.
Publish your numbers — "make Spegel look good" is an explicit project
goal, and reproducible benchmarks are how.

## Notes

- **Same story, Dragonfly**: point the `url` at a registry fronted by
  Dragonfly and the P2P layer swaps out; documented but not the flagship.
- **Replication**: pair `replicas: [zot-cluster-b]` on the route with
  `replication.spool_dir` to mirror pushes across clusters; replicas
  reconcile asynchronously with retries (admin API shows queue state).
- **Registry GC**: Shpiel tags commits and refs but never deletes layers;
  configure Zot's garbage collection / retention to taste.
