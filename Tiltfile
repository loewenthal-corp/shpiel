# Tiltfile for the shpiel development environment
# tilt.dev
#
# Prerequisites:
#   - Local Kubernetes cluster (e.g., OrbStack, Docker Desktop, kind)
#   - tilt (managed by Hermit: source bin/activate-hermit)
#
# Usage:
#   tilt up
#
# The environment is hermetic: shpiel pulls through from an in-cluster
# fakehub (a huggingface.co simulator seeded with fixture models), and the
# hf-smoke job proves an unmodified `hf download` works with only
# HF_ENDPOINT set. Zot is deployed as the target for the upcoming OCI
# backend (M1).

# --- Infrastructure ---

k8s_yaml('scripts/k8s_dev/namespaces.yaml')

k8s_yaml('scripts/k8s_dev/zot.yaml')
k8s_resource('zot', port_forwards='5000:5000', labels=['infra'])

# --- Fakehub (hermetic upstream) ---

docker_build(
    'fakehub',
    context='.',
    dockerfile='containers/fakehub.Dockerfile',
    only=['go.mod', 'go.sum', 'cmd/fakehub', 'internal'],
)
k8s_yaml(kustomize('kustomize/fakehub/dev'))
k8s_resource('fakehub', port_forwards='8081:8081', labels=['infra'])

# --- Shpiel ---

docker_build(
    'shpiel',
    context='.',
    dockerfile='containers/shpiel.Dockerfile',
    only=['go.mod', 'go.sum', 'cmd/shpiel', 'internal'],
)
k8s_yaml(kustomize('kustomize/shpiel/dev'))
k8s_resource(
    'shpiel',
    port_forwards=['8080:8080', '9090:9090'],
    resource_deps=['fakehub'],
    labels=['app'],
)

# --- Smoke test: real hf CLI against shpiel ---

docker_build(
    'hf-client',
    context='containers',
    dockerfile='containers/hf-client.Dockerfile',
)
k8s_yaml('scripts/k8s_dev/smoke.yaml')
k8s_resource(
    'hf-smoke',
    resource_deps=['shpiel'],
    trigger_mode=TRIGGER_MODE_MANUAL,
    auto_init=True,
    labels=['test'],
)

# --- Fast feedback: unit tests + lint on save ---

local_resource(
    'go-test',
    cmd='task test',
    deps=['cmd', 'internal', 'test/conformance', 'go.mod', 'go.sum'],
    allow_parallel=True,
    labels=['test'],
)

local_resource(
    'go-lint',
    cmd='task lint',
    deps=['cmd', 'internal', 'test', '.golangci.yml'],
    allow_parallel=True,
    labels=['test'],
)
