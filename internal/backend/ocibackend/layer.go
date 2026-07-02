package ocibackend

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/ociclient"
)

// --- deterministic tar layers ---

// pushTarLayerNamed streams a deterministic single-file tar layer:
// identical (name, content) always produces identical layer bytes, so
// content-addressed dedup holds across pushes. Returns the layer
// descriptor (offset annotation included), the content offset within the
// layer, and the content size.
func (b *Backend) pushTarLayerNamed(ctx context.Context, ociRepo, name string, content io.Reader, size int64) (ociclient.Descriptor, int64, int64, error) {
	if size < 0 {
		return ociclient.Descriptor{}, 0, 0, errors.New("ocibackend: tar layers require a known content size")
	}
	w, err := b.client.NewBlobWriter(ctx, ociRepo)
	if err != nil {
		return ociclient.Descriptor{}, 0, 0, err
	}

	counting := &countingWriter{w: w}
	tw := tar.NewWriter(counting)
	hdr := &tar.Header{
		Name:    name,
		Mode:    0o644,
		Size:    size,
		ModTime: time.Unix(0, 0),
		Uid:     0,
		Gid:     0,
		Format:  tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return ociclient.Descriptor{}, 0, 0, fmt.Errorf("ocibackend: writing tar header: %w", err)
	}
	// Everything before the content is header bytes; that count is the
	// content offset, whatever tar format quirks apply.
	offset := counting.n

	written, err := io.Copy(tw, content)
	if err != nil {
		return ociclient.Descriptor{}, 0, 0, fmt.Errorf("ocibackend: writing tar content: %w", err)
	}
	if written != size {
		return ociclient.Descriptor{}, 0, 0, fmt.Errorf("ocibackend: tar content short write: got %d, want %d", written, size)
	}
	if err := tw.Close(); err != nil {
		return ociclient.Descriptor{}, 0, 0, fmt.Errorf("ocibackend: closing tar: %w", err)
	}

	digest, layerSize, err := w.Commit()
	if err != nil {
		return ociclient.Descriptor{}, 0, 0, err
	}
	desc := ociclient.Descriptor{
		MediaType:   ociclient.MediaTypeOCILayerTar,
		Digest:      digest,
		Size:        layerSize,
		Annotations: map[string]string{annoOffset: strconv.FormatInt(offset, 10)},
	}
	return desc, offset, size, nil
}

// pushTarLayer is pushTarLayerNamed for callers that only need the
// descriptor.
func (b *Backend) pushTarLayer(ctx context.Context, ociRepo, name string, content io.Reader, size int64) (ociclient.Descriptor, error) {
	desc, _, _, err := b.pushTarLayerNamed(ctx, ociRepo, name, content, size)
	return desc, err
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// --- per-repo blob index (tar-layers format) ---

// indexEntry locates one content digest inside a tar layer.
type indexEntry struct {
	Layer     string `json:"layer"`
	LayerSize int64  `json:"layerSize"`
	Offset    int64  `json:"offset"`
	Size      int64  `json:"size"`
}

// blobIndex maps content sha256 hex -> location. It lives in the registry
// as its own tagged artifact, so it survives restarts and is replicated
// with the repo.
type blobIndex struct {
	Blobs map[string]indexEntry `json:"blobs"`
}

func (b *Backend) loadIndex(ctx context.Context, ociRepo string) (*blobIndex, error) {
	om, err := b.client.GetManifest(ctx, ociRepo, indexTag)
	if errors.Is(err, ociclient.ErrNotFound) {
		return &blobIndex{Blobs: map[string]indexEntry{}}, nil
	}
	if err != nil {
		return nil, err
	}
	rc, err := b.client.GetBlob(ctx, ociRepo, om.Config.Digest, 0)
	if err != nil {
		return nil, fmt.Errorf("ocibackend: fetching blob index: %w", err)
	}
	defer rc.Close()
	var idx blobIndex
	if err := json.NewDecoder(io.LimitReader(rc, 256<<20)).Decode(&idx); err != nil {
		return nil, fmt.Errorf("ocibackend: decoding blob index: %w", err)
	}
	if idx.Blobs == nil {
		idx.Blobs = map[string]indexEntry{}
	}
	return &idx, nil
}

func (b *Backend) saveIndex(ctx context.Context, ociRepo string, idx *blobIndex) error {
	payload, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	configDesc, err := b.pushBlobBytes(ctx, ociRepo, payload, mediaTypeShpielIndex)
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
		ArtifactType:  mediaTypeShpielIndex,
		Config:        configDesc,
		Layers:        []ociclient.Descriptor{emptyDesc},
	}
	_, err = b.client.PutManifest(ctx, ociRepo, indexTag, om)
	return err
}

// --- ranged blob reader ---

// blobReader exposes a window of a registry blob as an io.ReadSeekCloser.
// Seeks translate into ranged GETs, so http.ServeContent's Range handling
// works against registry-backed content without buffering.
type blobReader struct {
	ctx    context.Context
	client *ociclient.Client
	repo   string
	layer  string
	base   int64 // content start within the blob
	size   int64 // content size
	pos    int64
	cur    io.ReadCloser
}

func (r *blobReader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if r.cur == nil {
		rc, err := r.client.GetBlob(r.ctx, r.repo, r.layer, r.base+r.pos)
		if err != nil {
			return 0, err
		}
		r.cur = struct {
			io.Reader
			io.Closer
		}{io.LimitReader(rc, r.size-r.pos), rc}
	}
	n, err := r.cur.Read(p)
	r.pos += int64(n)
	if errors.Is(err, io.EOF) && r.pos < r.size {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *blobReader) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = r.pos + offset
	case io.SeekEnd:
		next = r.size + offset
	default:
		return 0, fmt.Errorf("ocibackend: invalid seek whence %d", whence)
	}
	if next < 0 {
		return 0, fmt.Errorf("ocibackend: negative seek position %d", next)
	}
	if next != r.pos && r.cur != nil {
		_ = r.cur.Close()
		r.cur = nil
	}
	r.pos = next
	return next, nil
}

func (r *blobReader) Close() error {
	if r.cur != nil {
		return r.cur.Close()
	}
	return nil
}
