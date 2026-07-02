package backend

import (
	"fmt"
	"regexp"
	"strings"
)

// Digest is a content address in "<algo>:<hex>" form, e.g.
// "sha256:9f6e6800cf...". The algorithm prefix keeps sha1 git OIDs (regular
// files) and sha256 LFS OIDs (large files) in one keyspace.
type Digest string

var digestPattern = regexp.MustCompile(`^(sha1|sha256):([0-9a-f]{6,128})$`)

// ParseDigest validates s as a digest.
func ParseDigest(s string) (Digest, error) {
	if !digestPattern.MatchString(s) {
		return "", fmt.Errorf("invalid digest %q: want <algo>:<hex>", s)
	}
	return Digest(s), nil
}

// NewDigest builds a digest from an algorithm and hex string.
func NewDigest(algo, hex string) Digest {
	return Digest(algo + ":" + strings.ToLower(hex))
}

// SHA256Digest builds a sha256 digest from a hex string.
func SHA256Digest(hex string) Digest { return NewDigest("sha256", hex) }

// SHA1Digest builds a sha1 digest from a hex string.
func SHA1Digest(hex string) Digest { return NewDigest("sha1", hex) }

// Algo returns the algorithm part, or "" for a malformed digest.
func (d Digest) Algo() string {
	algo, _, ok := strings.Cut(string(d), ":")
	if !ok {
		return ""
	}
	return algo
}

// Hex returns the hex part, or the whole string if there is no prefix.
func (d Digest) Hex() string {
	_, hex, ok := strings.Cut(string(d), ":")
	if !ok {
		return string(d)
	}
	return hex
}

// IsZero reports whether the digest is empty.
func (d Digest) IsZero() bool { return d == "" }

func (d Digest) String() string { return string(d) }
