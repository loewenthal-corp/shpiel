package ocibackend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
	"github.com/loewenthal-corp/shpiel/internal/ociclient"
)

// PutManifest implements backend.Backend. If every referenced blob is
// already in the registry the commit is published under its final tags;
// otherwise it is staged and promoted as blobs arrive.
func (b *Backend) PutManifest(ctx context.Context, m *backend.Manifest, refs map[string]string) error {
	if m.Repo.IsZero() || m.CommitSHA == "" {
		return errors.New("ocibackend: manifest requires repo and commit SHA")
	}
	kind := m.Kind
	if kind == "" {
		kind = hfapi.RepoKindModel
	}
	ociRepo, err := b.ociRepository(kind, m.Repo)
	if err != nil {
		return err
	}
	unlock := b.lockRepo(ociRepo)
	defer unlock()
	return b.putOrStage(ctx, ociRepo, kind, m, refsFor(refs, m.CommitSHA))
}

// refsFor filters a ref map down to names pointing at this commit.
func refsFor(refs map[string]string, commitSHA string) []string {
	var out []string
	for ref, sha := range refs {
		if sha == commitSHA {
			out = append(out, ref)
		}
	}
	sort.Strings(out)
	return out
}

// putOrStage publishes the commit if complete, else stages it. Caller
// holds the repo lock.
func (b *Backend) putOrStage(ctx context.Context, ociRepo string, kind hfapi.RepoKind, m *backend.Manifest, refs []string) error {
	manifestBlob, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("ocibackend: encoding shpiel manifest: %w", err)
	}

	missing, layers, err := b.layersFor(ctx, ociRepo, m)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return b.stage(ctx, ociRepo, kind, m, manifestBlob, refs)
	}
	return b.publish(ctx, ociRepo, kind, m, manifestBlob, layers, refs)
}

// layersFor resolves manifest entries to layer descriptors, reporting the
// file digests not yet uploaded.
func (b *Backend) layersFor(ctx context.Context, ociRepo string, m *backend.Manifest) (missing []string, layers []ociclient.Descriptor, err error) {
	var idx *blobIndex
	if b.format == FormatTarLayers {
		idx, err = b.loadIndex(ctx, ociRepo)
		if err != nil {
			return nil, nil, err
		}
	}
	for i := range m.Files {
		f := &m.Files[i]
		if f.Digest.IsZero() || f.Digest.Algo() != "sha256" {
			// No digest yet, or a git-sha1 key from an upstream listing
			// whose blob hasn't been fetched: the relay re-keys such
			// entries to sha256 when the content arrives, at which point
			// this commit becomes promotable.
			missing = append(missing, f.Path)
			continue
		}
		switch b.format {
		case FormatModelPack:
			ociDigest := "sha256:" + f.Digest.Hex()
			size, err := b.client.HeadBlob(ctx, ociRepo, ociDigest)
			if errors.Is(err, ociclient.ErrNotFound) {
				missing = append(missing, f.Path)
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			layers = append(layers, ociclient.Descriptor{
				MediaType: mediaTypeModelWeightRaw,
				Digest:    ociDigest,
				Size:      size,
				Annotations: map[string]string{
					annoTitle:      f.Path,
					annoFileDigest: f.Digest.String(),
				},
			})
		case FormatTarLayers:
			entry, ok := idx.Blobs[f.Digest.Hex()]
			if !ok {
				missing = append(missing, f.Path)
				continue
			}
			layers = append(layers, ociclient.Descriptor{
				MediaType: ociclient.MediaTypeOCILayerTar,
				Digest:    entry.Layer,
				Size:      entry.LayerSize,
				Annotations: map[string]string{
					annoTitle:      f.Path,
					annoFileDigest: f.Digest.String(),
					annoOffset:     strconv.FormatInt(entry.Offset, 10),
				},
			})
		}
	}
	return missing, layers, nil
}

// publish pushes the final OCI manifest under the commit tag and all refs.
func (b *Backend) publish(ctx context.Context, ociRepo string, kind hfapi.RepoKind, m *backend.Manifest, manifestBlob []byte, layers []ociclient.Descriptor, refs []string) error {
	annotations := map[string]string{
		annoCommit: m.CommitSHA,
		annoRepo:   m.Repo.String(),
		annoKind:   string(kind),
	}

	var om *ociclient.Manifest
	switch b.format {
	case FormatModelPack:
		configDesc, err := b.pushBlobBytes(ctx, ociRepo, manifestBlob, mediaTypeShpielManifest)
		if err != nil {
			return err
		}
		if len(layers) == 0 {
			// The OCI schema requires a layers array; empty commits (repo
			// creation) get the standard empty-JSON placeholder.
			emptyDesc, err := b.pushBlobBytes(ctx, ociRepo, emptyJSON, ociclient.MediaTypeEmptyJSON)
			if err != nil {
				return err
			}
			layers = []ociclient.Descriptor{emptyDesc}
		}
		om = &ociclient.Manifest{
			SchemaVersion: 2,
			MediaType:     ociclient.MediaTypeOCIManifest,
			ArtifactType:  artifactTypeModel,
			Config:        configDesc,
			Layers:        layers,
			Annotations:   annotations,
		}
	case FormatTarLayers:
		// The shpiel manifest rides as one more tar layer so the artifact
		// stays a plain mountable image.
		manifestLayer, err := b.pushTarLayer(ctx, ociRepo, ".shpiel/manifest.json", bytes.NewReader(manifestBlob), int64(len(manifestBlob)))
		if err != nil {
			return err
		}
		manifestLayer.Annotations[annoRole] = roleManifest
		layers = append(layers, manifestLayer)

		configDesc, err := b.pushImageConfig(ctx, ociRepo, layers)
		if err != nil {
			return err
		}
		om = &ociclient.Manifest{
			SchemaVersion: 2,
			MediaType:     ociclient.MediaTypeOCIManifest,
			Config:        configDesc,
			Layers:        layers,
			Annotations:   annotations,
		}
	}

	if _, err := b.client.PutManifest(ctx, ociRepo, m.CommitSHA, om); err != nil {
		return err
	}
	for _, ref := range refs {
		if _, err := b.client.PutManifest(ctx, ociRepo, tagForRef(ref), om); err != nil {
			return fmt.Errorf("ocibackend: tagging ref %s: %w", ref, err)
		}
	}
	// Best-effort: drop the staged tag now that the real one exists.
	_ = b.client.DeleteManifest(ctx, ociRepo, stagedTagPrefix+m.CommitSHA)
	return nil
}

// stage records an incomplete commit in the registry: a manifest whose
// config is the shpiel manifest, under a reserved staged tag carrying the
// refs to apply on promotion.
func (b *Backend) stage(ctx context.Context, ociRepo string, kind hfapi.RepoKind, m *backend.Manifest, manifestBlob []byte, refs []string) error {
	// Merge refs with any recorded by a previous staging of this commit.
	stagedTag := stagedTagPrefix + m.CommitSHA
	if prev, err := b.client.GetManifest(ctx, ociRepo, stagedTag); err == nil {
		refs = mergeRefs(refs, strings.Split(prev.Annotations[annoRefs], ","))
	}

	configDesc, err := b.pushBlobBytes(ctx, ociRepo, manifestBlob, mediaTypeShpielManifest)
	if err != nil {
		return err
	}
	emptyDesc, err := b.pushBlobBytes(ctx, ociRepo, emptyJSON, ociclient.MediaTypeEmptyJSON)
	if err != nil {
		return err
	}
	om := &ociclient.Manifest{
		SchemaVersion: 2,
		MediaType:     ociclient.MediaTypeOCIManifest,
		ArtifactType:  mediaTypeShpielManifest,
		Config:        configDesc,
		Layers:        []ociclient.Descriptor{emptyDesc},
		Annotations: map[string]string{
			annoCommit: m.CommitSHA,
			annoRepo:   m.Repo.String(),
			annoKind:   string(kind),
			annoRefs:   strings.Join(refs, ","),
		},
	}
	if _, err := b.client.PutManifest(ctx, ociRepo, stagedTag, om); err != nil {
		return err
	}
	b.mu.Lock()
	b.pending[ociRepo+"|"+string(kind)] = true
	b.mu.Unlock()
	return nil
}

func mergeRefs(a, bRefs []string) []string {
	set := map[string]bool{}
	for _, r := range append(append([]string{}, a...), bRefs...) {
		if r != "" {
			set[r] = true
		}
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// promoteStaged retries publication of every staged commit in a repo;
// called after blob arrivals. Caller holds the repo lock.
func (b *Backend) promoteStaged(ctx context.Context, ociRepo string, kind hfapi.RepoKind) error {
	tags, err := b.client.ListTags(ctx, ociRepo)
	if err != nil {
		return err
	}
	stagedLeft := false
	for _, tag := range tags {
		if !strings.HasPrefix(tag, stagedTagPrefix) {
			continue
		}
		om, err := b.client.GetManifest(ctx, ociRepo, tag)
		if err != nil {
			continue
		}
		m, err := b.shpielManifestFrom(ctx, ociRepo, om)
		if err != nil {
			continue
		}
		refs := strings.Split(om.Annotations[annoRefs], ",")
		manifestBlob, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			continue
		}
		missing, layers, err := b.layersFor(ctx, ociRepo, m)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			stagedLeft = true
			continue
		}
		if err := b.publish(ctx, ociRepo, kind, m, manifestBlob, layers, mergeRefs(nil, refs)); err != nil {
			return err
		}
	}
	if !stagedLeft {
		b.mu.Lock()
		delete(b.pending, ociRepo+"|"+string(kind))
		b.mu.Unlock()
	}
	return nil
}

// hasPending reports whether promotion work may exist for the repo,
// rebuilding lazily from registry tags after a restart.
func (b *Backend) hasPending(ctx context.Context, ociRepo string, kind hfapi.RepoKind) bool {
	b.mu.Lock()
	known := b.pending[ociRepo+"|"+string(kind)]
	b.mu.Unlock()
	if known {
		return true
	}
	tags, err := b.client.ListTags(ctx, ociRepo)
	if err != nil {
		return false
	}
	for _, tag := range tags {
		if strings.HasPrefix(tag, stagedTagPrefix) {
			b.mu.Lock()
			b.pending[ociRepo+"|"+string(kind)] = true
			b.mu.Unlock()
			return true
		}
	}
	return false
}

// pushBlobBytes uploads a small blob and returns its descriptor.
func (b *Backend) pushBlobBytes(ctx context.Context, ociRepo string, content []byte, mediaType string) (ociclient.Descriptor, error) {
	digest := ociclient.SHA256Digest(content)
	if err := b.client.PutBlob(ctx, ociRepo, digest, bytes.NewReader(content), int64(len(content))); err != nil {
		return ociclient.Descriptor{}, err
	}
	return ociclient.Descriptor{MediaType: mediaType, Digest: digest, Size: int64(len(content))}, nil
}

// pushImageConfig writes the OCI image config for a tar-layers artifact:
// uncompressed tar layers mean diff_ids equal the layer digests.
func (b *Backend) pushImageConfig(ctx context.Context, ociRepo string, layers []ociclient.Descriptor) (ociclient.Descriptor, error) {
	diffIDs := make([]string, len(layers))
	for i, l := range layers {
		diffIDs[i] = l.Digest
	}
	config, err := json.Marshal(map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"config":       map[string]any{},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": diffIDs},
	})
	if err != nil {
		return ociclient.Descriptor{}, err
	}
	return b.pushBlobBytes(ctx, ociRepo, config, ociclient.MediaTypeOCIConfig)
}

// StatBlob implements backend.Backend. Non-sha256 keys are simply absent
// (OCI cannot address them); the relay re-keys and refetches.
func (b *Backend) StatBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, digest backend.Digest) (backend.BlobInfo, error) {
	if digest.Algo() != "sha256" {
		return backend.BlobInfo{}, backend.ErrBlobNotFound
	}
	ociRepo, err := b.ociRepository(kind, repo)
	if err != nil {
		return backend.BlobInfo{}, err
	}
	switch b.format {
	case FormatTarLayers:
		idx, err := b.loadIndex(ctx, ociRepo)
		if err != nil {
			return backend.BlobInfo{}, err
		}
		entry, ok := idx.Blobs[digest.Hex()]
		if !ok {
			return backend.BlobInfo{}, backend.ErrBlobNotFound
		}
		return backend.BlobInfo{Digest: digest, Size: entry.Size}, nil
	default:
		size, err := b.client.HeadBlob(ctx, ociRepo, "sha256:"+digest.Hex())
		if errors.Is(err, ociclient.ErrNotFound) {
			return backend.BlobInfo{}, backend.ErrBlobNotFound
		}
		if err != nil {
			return backend.BlobInfo{}, err
		}
		return backend.BlobInfo{Digest: digest, Size: size}, nil
	}
}

// OpenBlob implements backend.Backend with a lazy ranged reader, so HTTP
// Range requests against Shpiel translate to ranged blob GETs against the
// registry.
func (b *Backend) OpenBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, digest backend.Digest) (io.ReadSeekCloser, error) {
	if digest.Algo() != "sha256" {
		return nil, backend.ErrBlobNotFound
	}
	ociRepo, err := b.ociRepository(kind, repo)
	if err != nil {
		return nil, err
	}
	switch b.format {
	case FormatTarLayers:
		idx, err := b.loadIndex(ctx, ociRepo)
		if err != nil {
			return nil, err
		}
		entry, ok := idx.Blobs[digest.Hex()]
		if !ok {
			return nil, backend.ErrBlobNotFound
		}
		return &blobReader{ctx: ctx, client: b.client, repo: ociRepo, layer: entry.Layer, base: entry.Offset, size: entry.Size}, nil
	default:
		ociDigest := "sha256:" + digest.Hex()
		size, err := b.client.HeadBlob(ctx, ociRepo, ociDigest)
		if errors.Is(err, ociclient.ErrNotFound) {
			return nil, backend.ErrBlobNotFound
		}
		if err != nil {
			return nil, err
		}
		return &blobReader{ctx: ctx, client: b.client, repo: ociRepo, layer: ociDigest, base: 0, size: size}, nil
	}
}

// PutBlob implements backend.Backend. Content is verified against digest
// while streaming; in tar-layers format the bytes are wrapped in a
// deterministic single-file tar and the digest index updated.
func (b *Backend) PutBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, digest backend.Digest, r io.Reader, size int64) error {
	if digest.Algo() != "sha256" {
		return fmt.Errorf("ocibackend: blob key %s: OCI backends require sha256 storage keys", digest)
	}
	ociRepo, err := b.ociRepository(kind, repo)
	if err != nil {
		return err
	}

	switch b.format {
	case FormatTarLayers:
		unlock := b.lockRepo(ociRepo)
		idx, err := b.loadIndex(ctx, ociRepo)
		if err != nil {
			unlock()
			return err
		}
		if _, ok := idx.Blobs[digest.Hex()]; ok {
			unlock()
			return nil // content-addressed: already have it
		}
		unlock()

		// Stream the tar build + upload outside the lock; only the index
		// update below needs it.
		verified := newVerifyingReader(r, digest.Hex())
		layerDesc, offset, contentSize, err := b.pushTarLayerNamed(ctx, ociRepo, blobFileName(digest), verified, size)
		if err != nil {
			return err
		}
		if err := verified.check(); err != nil {
			return err
		}

		unlock = b.lockRepo(ociRepo)
		defer unlock()
		idx, err = b.loadIndex(ctx, ociRepo)
		if err != nil {
			return err
		}
		idx.Blobs[digest.Hex()] = indexEntry{Layer: layerDesc.Digest, LayerSize: layerDesc.Size, Offset: offset, Size: contentSize}
		if err := b.saveIndex(ctx, ociRepo, idx); err != nil {
			return err
		}
		if b.hasPending(ctx, ociRepo, kind) {
			return b.promoteStaged(ctx, ociRepo, kind)
		}
		return nil

	default:
		ociDigest := "sha256:" + digest.Hex()
		if err := b.client.PutBlob(ctx, ociRepo, ociDigest, r, size); err != nil {
			return err
		}
		if b.hasPending(ctx, ociRepo, kind) {
			unlock := b.lockRepo(ociRepo)
			defer unlock()
			return b.promoteStaged(ctx, ociRepo, kind)
		}
		return nil
	}
}

// blobFileName is the in-tar file name for content-addressed tar layers.
// The real repo path lands in layer annotations; the tar member name stays
// stable so identical content produces identical layers (dedup).
func blobFileName(digest backend.Digest) string {
	return "blobs/" + digest.Hex()
}

// verifyingReader hashes content as it flows and checks the sum afterward.
type verifyingReader struct {
	r      io.Reader
	hasher interface {
		Write(p []byte) (int, error)
		Sum(b []byte) []byte
	}
	want string
}

func newVerifyingReader(r io.Reader, wantHex string) *verifyingReader {
	h := sha256.New()
	return &verifyingReader{r: io.TeeReader(r, h), hasher: h, want: wantHex}
}

func (v *verifyingReader) Read(p []byte) (int, error) { return v.r.Read(p) }

func (v *verifyingReader) check() error {
	if got := hex.EncodeToString(v.hasher.Sum(nil)); got != v.want {
		return fmt.Errorf("%w: got sha256:%s, want sha256:%s", backend.ErrDigestMismatch, got, v.want)
	}
	return nil
}
