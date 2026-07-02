// Package ocibackend stores model repos as OCI artifacts in a registry
// (Zot, Harbor, GHCR, ...): one OCI repository per model repo, one OCI
// manifest per commit (tagged by commit SHA), refs as human tags, and one
// layer per file. This is the flagship deployment target: weights in an
// in-cluster registry are mountable via Kubernetes image volumes and
// distributable peer-to-peer by Spegel.
//
// Two formats:
//
//   - modelpack (default): layers carry raw file bytes, so the layer
//     digest IS the file's content sha256 — dedup and lookups are direct.
//     ModelPack/modctl-style artifact.
//   - tar-layers: each layer is a deterministic tar of one file, media
//     type application/vnd.oci.image.layer.v1.tar with a proper OCI image
//     config, so containerd image volumes mount the artifact natively.
//     File content sits at a fixed offset inside the layer; a per-repo
//     index artifact maps content digests to (layer, offset).
//
// Commits whose blobs have not all arrived yet (pull-through fills blobs
// lazily) are staged under a reserved tag and promoted to their real tags
// when the last blob lands. Staging state lives in the registry itself, so
// Shpiel stays restart-safe with no local database.
package ocibackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
	"github.com/loewenthal-corp/shpiel/internal/ociclient"
)

// Formats.
const (
	FormatModelPack = "modelpack"
	FormatTarLayers = "tar-layers"
)

// Media types and annotations of the Shpiel OCI mapping.
const (
	mediaTypeShpielManifest = "application/vnd.shpiel.manifest.v1+json"
	mediaTypeShpielIndex    = "application/vnd.shpiel.blob-index.v1+json"
	artifactTypeModel       = "application/vnd.cncf.model.manifest.v1+json"
	mediaTypeModelWeightRaw = "application/vnd.cncf.model.weight.v1.raw"

	annoCommit     = "org.shpiel.commit"
	annoRepo       = "org.shpiel.repo"
	annoKind       = "org.shpiel.kind"
	annoRefs       = "org.shpiel.refs"
	annoRole       = "org.shpiel.role"
	annoFileDigest = "org.shpiel.file.digest"
	annoOffset     = "org.shpiel.content.offset"
	annoTitle      = "org.opencontainers.image.title"

	roleManifest = "shpiel-manifest"

	stagedTagPrefix = "shpiel-staged-"
	indexTag        = "shpiel-index"
)

var emptyJSON = []byte("{}")

// Options configure an OCI backend instance.
type Options struct {
	// URL of the registry, scheme included (http:// for in-cluster plain
	// registries).
	URL string
	// Format is modelpack (default) or tar-layers.
	Format string
	// RepoPrefix prepends a path to every OCI repository name, e.g.
	// "shpiel" -> shpiel/models/org/name.
	RepoPrefix string
	// Username and Password authenticate pushes/pulls; empty = anonymous.
	Username string
	Password string
}

// Backend implements backend.Backend on an OCI registry.
type Backend struct {
	name   string
	client *ociclient.Client
	format string
	prefix string

	mu sync.Mutex
	// pending tracks repos with staged commits so PutBlob knows when a
	// promotion attempt is worthwhile. Rebuilt lazily from registry tags,
	// so restarts lose nothing.
	pending map[string]bool
	// repoLocks serialize manifest/index mutations per repo.
	repoLocks sync.Map
}

var _ backend.Backend = (*Backend)(nil)

// New creates an OCI backend.
func New(name string, opts Options) (*Backend, error) {
	format := opts.Format
	if format == "" {
		format = FormatModelPack
	}
	if format != FormatModelPack && format != FormatTarLayers {
		return nil, fmt.Errorf("ocibackend: unknown format %q (want %s or %s)", format, FormatModelPack, FormatTarLayers)
	}
	client, err := ociclient.New(opts.URL, opts.Username, opts.Password)
	if err != nil {
		return nil, err
	}
	return &Backend{
		name:    name,
		client:  client,
		format:  format,
		prefix:  strings.Trim(opts.RepoPrefix, "/"),
		pending: map[string]bool{},
	}, nil
}

// Name implements backend.Backend.
func (b *Backend) Name() string { return b.name }

// Format returns the configured artifact format.
func (b *Backend) Format() string { return b.format }

var ociRepoSegment = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// ociRepository maps an HF repo id onto an OCI repository name:
// [prefix/]models/{owner}/{name}, lowercased (OCI names are lowercase-only;
// HF ids that collide after lowercasing share a repository — documented
// limitation).
func (b *Backend) ociRepository(kind hfapi.RepoKind, repo hfapi.RepoID) (string, error) {
	name := strings.ToLower(path.Join(b.prefix, kind.APIPrefix(), repo.String()))
	for _, seg := range strings.Split(name, "/") {
		if !ociRepoSegment.MatchString(seg) {
			return "", fmt.Errorf("ocibackend: repo id %q maps to invalid OCI segment %q", repo, seg)
		}
	}
	return name, nil
}

var ociTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// tagForRef renders a ref as an OCI tag: verbatim when legal, slashes
// flattened otherwise (refs/pr/1 -> refs-pr-1).
func tagForRef(ref string) string {
	if ociTagPattern.MatchString(ref) {
		return ref
	}
	sanitized := strings.NewReplacer("/", "-", ":", "-", "@", "-").Replace(ref)
	if !ociTagPattern.MatchString(sanitized) {
		sanitized = "ref-" + strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
				return r
			default:
				return '-'
			}
		}, sanitized)
	}
	return sanitized
}

func (b *Backend) lockRepo(ociRepo string) func() {
	v, _ := b.repoLocks.LoadOrStore(ociRepo, &sync.Mutex{})
	mu, ok := v.(*sync.Mutex)
	if !ok {
		panic("ocibackend: repoLocks holds a non-mutex")
	}
	mu.Lock()
	return mu.Unlock
}

// CreateRepo implements backend.Backend. OCI registries create
// repositories implicitly on first push; existence is "has any tags".
func (b *Backend) CreateRepo(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID) error {
	ociRepo, err := b.ociRepository(kind, repo)
	if err != nil {
		return err
	}
	tags, err := b.client.ListTags(ctx, ociRepo)
	if err != nil {
		return err
	}
	if len(tags) > 0 {
		return backend.ErrRepoExists
	}
	return nil
}

// DeleteRepo implements backend.Backend by deleting every tag; blob GC is
// the registry's job.
func (b *Backend) DeleteRepo(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID) error {
	ociRepo, err := b.ociRepository(kind, repo)
	if err != nil {
		return err
	}
	unlock := b.lockRepo(ociRepo)
	defer unlock()
	tags, err := b.client.ListTags(ctx, ociRepo)
	if err != nil {
		return err
	}
	if len(tags) == 0 {
		return backend.ErrRepoNotFound
	}
	var errs []error
	for _, tag := range tags {
		if err := b.client.DeleteManifest(ctx, ociRepo, tag); err != nil && !errors.Is(err, ociclient.ErrNotFound) {
			errs = append(errs, fmt.Errorf("tag %s: %w", tag, err))
		}
	}
	return errors.Join(errs...)
}

// ResolveRef implements backend.Backend.
func (b *Backend) ResolveRef(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, ref string) (string, error) {
	ociRepo, err := b.ociRepository(kind, repo)
	if err != nil {
		return "", err
	}
	if hfapi.IsCommitSHA(ref) {
		if _, err := b.GetManifest(ctx, kind, repo, ref); err != nil {
			return "", err
		}
		return ref, nil
	}
	m, err := b.client.GetManifest(ctx, ociRepo, tagForRef(ref))
	if err != nil {
		if errors.Is(err, ociclient.ErrNotFound) {
			return "", b.missErr(ctx, ociRepo)
		}
		return "", err
	}
	commit := m.Annotations[annoCommit]
	if commit == "" {
		return "", fmt.Errorf("ocibackend: tag %s/%s has no %s annotation", ociRepo, ref, annoCommit)
	}
	return commit, nil
}

// missErr distinguishes RepoNotFound from RevisionNotFound by whether the
// repository has any tags at all.
func (b *Backend) missErr(ctx context.Context, ociRepo string) error {
	tags, err := b.client.ListTags(ctx, ociRepo)
	if err != nil || len(tags) == 0 {
		return backend.ErrRepoNotFound
	}
	return backend.ErrRevisionNotFound
}

// GetManifest implements backend.Backend: promoted commits first, staged
// commits second (their file list is complete even when blobs are still
// arriving, which is exactly what pull-through needs).
func (b *Backend) GetManifest(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, commitSHA string) (*backend.Manifest, error) {
	ociRepo, err := b.ociRepository(kind, repo)
	if err != nil {
		return nil, err
	}
	for _, tag := range []string{commitSHA, stagedTagPrefix + commitSHA} {
		om, err := b.client.GetManifest(ctx, ociRepo, tag)
		if errors.Is(err, ociclient.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return b.shpielManifestFrom(ctx, ociRepo, om)
	}
	return nil, b.missErr(ctx, ociRepo)
}

// shpielManifestFrom extracts the embedded Shpiel manifest from an OCI
// manifest, wherever the format put it.
func (b *Backend) shpielManifestFrom(ctx context.Context, ociRepo string, om *ociclient.Manifest) (*backend.Manifest, error) {
	var rc io.ReadCloser
	switch om.Config.MediaType {
	case mediaTypeShpielManifest:
		var err error
		rc, err = b.client.GetBlob(ctx, ociRepo, om.Config.Digest, 0)
		if err != nil {
			return nil, fmt.Errorf("ocibackend: fetching manifest config blob: %w", err)
		}
	default:
		layer := findLayer(om, func(d ociclient.Descriptor) bool { return d.Annotations[annoRole] == roleManifest })
		if layer == nil {
			return nil, fmt.Errorf("ocibackend: OCI manifest carries no shpiel manifest")
		}
		offset := parseOffset(layer.Annotations[annoOffset])
		var err error
		rc, err = b.client.GetBlob(ctx, ociRepo, layer.Digest, offset)
		if err != nil {
			return nil, fmt.Errorf("ocibackend: fetching manifest layer: %w", err)
		}
	}
	defer rc.Close()
	var m backend.Manifest
	if err := json.NewDecoder(io.LimitReader(rc, 64<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("ocibackend: decoding shpiel manifest: %w", err)
	}
	return &m, nil
}

func findLayer(m *ociclient.Manifest, pred func(ociclient.Descriptor) bool) *ociclient.Descriptor {
	for i := range m.Layers {
		if pred(m.Layers[i]) {
			return &m.Layers[i]
		}
	}
	return nil
}

func parseOffset(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// Ping implements backend.Backend.
func (b *Backend) Ping(ctx context.Context) error {
	return b.client.Ping(ctx)
}
