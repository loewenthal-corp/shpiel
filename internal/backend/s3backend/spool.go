package s3backend

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

// spooledBlob is verified content staged on local disk, ready to PUT: the
// file is positioned at the start, the size is exact, and payloadSHA256 is
// the raw-content hash SigV4 signs.
type spooledBlob struct {
	file          *os.File
	size          int64
	payloadSHA256 string
}

func (s *spooledBlob) cleanup() {
	_ = s.file.Close()
	_ = os.Remove(s.file.Name())
}

// spoolAndVerify streams r to a temp file while checking the content
// against digest. sha256 digests hash the raw bytes (LFS); that hash
// doubles as the payload hash S3 PUTs sign. sha1 digests are git blob
// OIDs, which prepend a "blob <size>\x00" header — hashed by re-reading
// the spool once its length is known (sha1 keys are small regular files;
// the big LFS weights are all sha256).
func spoolAndVerify(r io.Reader, digest backend.Digest, size int64) (*spooledBlob, error) {
	tmp, err := os.CreateTemp("", "shpiel-s3-*")
	if err != nil {
		return nil, fmt.Errorf("s3backend: creating spool file: %w", err)
	}
	spooled := &spooledBlob{file: tmp}
	ok := false
	defer func() {
		if !ok {
			spooled.cleanup()
		}
	}()

	payload := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, payload), r)
	if err != nil {
		return nil, fmt.Errorf("s3backend: spooling blob %s: %w", digest, err)
	}
	spooled.size = written
	spooled.payloadSHA256 = hex.EncodeToString(payload.Sum(nil))

	switch digest.Algo() {
	case "sha256":
		if spooled.payloadSHA256 != digest.Hex() {
			return nil, fmt.Errorf("%w: got sha256:%s, want %s", backend.ErrDigestMismatch, spooled.payloadSHA256, digest)
		}
	case "sha1":
		git := gitBlobHasher(written)
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("s3backend: rewinding spool: %w", err)
		}
		if _, err := io.Copy(git, tmp); err != nil {
			return nil, fmt.Errorf("s3backend: re-reading spool: %w", err)
		}
		if got := hex.EncodeToString(git.Sum(nil)); got != digest.Hex() {
			return nil, fmt.Errorf("%w: got sha1:%s, want %s", backend.ErrDigestMismatch, got, digest)
		}
	}
	if size >= 0 && written != size {
		return nil, fmt.Errorf("s3backend: short write for %s: got %d bytes, want %d", digest, written, size)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("s3backend: rewinding spool: %w", err)
	}
	ok = true
	return spooled, nil
}

// gitBlobHasher returns a sha1 hash pre-seeded with the git blob header
// for the given content size.
func gitBlobHasher(size int64) hash.Hash {
	h := sha1.New()
	h.Write([]byte("blob " + strconv.FormatInt(size, 10) + "\x00"))
	return h
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
