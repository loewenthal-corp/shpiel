package fsbackend

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"strconv"

	"github.com/loewenthal-corp/shpiel/internal/backend"
)

// writeAndVerify streams r into f while checking the content against digest.
// sha256 digests hash the raw bytes (LFS). sha1 digests are git blob OIDs,
// which prepend a "blob <size>\x00" header — computable in-stream only when
// size is known upfront, so unknown-size sha1 content is verified by
// re-reading the temp file afterwards.
func writeAndVerify(f *os.File, r io.Reader, digest backend.Digest, size int64) (int64, error) {
	var h hash.Hash
	switch digest.Algo() {
	case "sha256":
		h = sha256.New()
	case "sha1":
		if size >= 0 {
			h = gitBlobHasher(size)
		}
	}

	var w io.Writer = f
	if h != nil {
		w = io.MultiWriter(f, h)
	}
	written, err := io.Copy(w, r)
	if err != nil {
		return written, fmt.Errorf("fsbackend: writing blob %s: %w", digest, err)
	}

	switch {
	case h != nil:
		if got := hex.EncodeToString(h.Sum(nil)); got != digest.Hex() {
			return written, fmt.Errorf("%w: got %s:%s, want %s", backend.ErrDigestMismatch, digest.Algo(), got, digest)
		}
	case digest.Algo() == "sha1":
		// Size was unknown: hash the temp file now that we know its length.
		if err := verifyGitBlobFile(f, written, digest); err != nil {
			return written, err
		}
	}
	return written, nil
}

// gitBlobHasher returns a sha1 hash pre-seeded with the git blob header for
// the given content size.
func gitBlobHasher(size int64) hash.Hash {
	h := sha1.New()
	h.Write([]byte("blob " + strconv.FormatInt(size, 10) + "\x00"))
	return h
}

func verifyGitBlobFile(f *os.File, size int64, digest backend.Digest) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("fsbackend: rewinding temp blob: %w", err)
	}
	h := gitBlobHasher(size)
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("fsbackend: re-reading temp blob: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest.Hex() {
		return fmt.Errorf("%w: got sha1:%s, want %s", backend.ErrDigestMismatch, got, digest)
	}
	return nil
}
