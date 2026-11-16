// Package integrity implements digest parsing, hashing and comparison helpers.
package integrity

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strings"
)

// ErrMismatch means a computed digest differs from an expected digest.
var ErrMismatch = errors.New("digest mismatch")

// Digest is a lowercase hexadecimal digest with its algorithm.
type Digest struct {
	Algo string
	Hex  string
}

// MismatchError describes one failed comparison.
type MismatchError struct {
	Algo string
	Want string
	Got  string
}

// Error implements error.
func (e *MismatchError) Error() string {
	return fmt.Sprintf("%s digest mismatch: expected %s, got %s", e.Algo, e.Want, e.Got)
}

// Is lets errors.Is match ErrMismatch.
func (e *MismatchError) Is(target error) bool { return target == ErrMismatch }

// AlgoSize returns the digest size in bytes for an algorithm.
func AlgoSize(algo string) (int, error) {
	switch strings.ToLower(algo) {
	case "sha256":
		return sha256.Size, nil
	case "sha512":
		return sha512.Size, nil
	case "sha1":
		return sha1.Size, nil
	case "md5":
		return md5.Size, nil
	default:
		return 0, fmt.Errorf("unsupported digest algorithm %q", algo)
	}
}

// ParseHex validates and normalizes a hexadecimal digest.
func ParseHex(algo, value string) (Digest, error) {
	algo = strings.ToLower(algo)
	size, err := AlgoSize(algo)
	if err != nil {
		return Digest{}, err
	}
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != size*2 {
		return Digest{}, fmt.Errorf("%s digest must be %d hex characters", algo, size*2)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return Digest{}, fmt.Errorf("%s digest is not hexadecimal", algo)
	}
	return Digest{Algo: algo, Hex: value}, nil
}

// ParseSRI parses one or more space-separated Subresource Integrity values.
func ParseSRI(value string) ([]Digest, error) {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return nil, fmt.Errorf("empty integrity value")
	}
	out := make([]Digest, 0, len(fields))
	for _, field := range fields {
		algo, encoded, ok := strings.Cut(field, "-")
		if !ok {
			return nil, fmt.Errorf("invalid integrity value %q", field)
		}
		algo = strings.ToLower(algo)
		size, err := AlgoSize(algo)
		if err != nil {
			return nil, err
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			raw, err = base64.RawStdEncoding.DecodeString(encoded)
			if err != nil {
				return nil, fmt.Errorf("invalid integrity value %q", field)
			}
		}
		if len(raw) != size {
			return nil, fmt.Errorf("%s integrity value has %d bytes, want %d", algo, len(raw), size)
		}
		out = append(out, Digest{Algo: algo, Hex: hex.EncodeToString(raw)})
	}
	return out, nil
}

// Hasher computes one or more digests while bytes are written.
type Hasher struct {
	hashes map[string]hash.Hash
}

// NewHasher returns a hasher for the requested algorithms.
func NewHasher(algos ...string) (*Hasher, error) {
	h := &Hasher{hashes: make(map[string]hash.Hash, len(algos))}
	for _, algo := range algos {
		algo = strings.ToLower(algo)
		if _, ok := h.hashes[algo]; ok {
			continue
		}
		hasher, err := newHash(algo)
		if err != nil {
			return nil, err
		}
		h.hashes[algo] = hasher
	}
	return h, nil
}

// Write adds bytes to every digest.
func (h *Hasher) Write(p []byte) (int, error) {
	for _, hasher := range h.hashes {
		_, _ = hasher.Write(p)
	}
	return len(p), nil
}

// Sum returns the lowercase hexadecimal digests.
func (h *Hasher) Sum() map[string]string {
	out := make(map[string]string, len(h.hashes))
	for algo, hasher := range h.hashes {
		out[algo] = hex.EncodeToString(hasher.Sum(nil))
	}
	return out
}

// Check verifies every expected digest against the actual set.
func Check(expected []Digest, actual map[string]string) error {
	for _, want := range expected {
		got, ok := actual[strings.ToLower(want.Algo)]
		if !ok {
			return fmt.Errorf("%w: %s digest was not computed", ErrMismatch, want.Algo)
		}
		if !strings.EqualFold(got, want.Hex) {
			return &MismatchError{Algo: want.Algo, Want: want.Hex, Got: got}
		}
	}
	return nil
}

// DigestsFromMap converts computed digests to parsed Digest values.
func DigestsFromMap(actual map[string]string) []Digest {
	out := make([]Digest, 0, len(actual))
	for algo, value := range actual {
		out = append(out, Digest{Algo: algo, Hex: value})
	}
	return out
}

func newHash(algo string) (hash.Hash, error) {
	switch strings.ToLower(algo) {
	case "sha256":
		return sha256.New(), nil
	case "sha512":
		return sha512.New(), nil
	case "sha1":
		return sha1.New(), nil
	case "md5":
		return md5.New(), nil
	default:
		return nil, fmt.Errorf("unsupported digest algorithm %q", algo)
	}
}
