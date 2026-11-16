package integrity

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestParseHex(t *testing.T) {
	want := strings.Repeat("ab", 32)
	got, err := ParseHex("SHA256", strings.ToUpper(want))
	if err != nil {
		t.Fatal(err)
	}
	if got.Algo != "sha256" || got.Hex != want {
		t.Fatalf("digest = %#v", got)
	}
	if _, err := ParseHex("sha256", "abcd"); err == nil {
		t.Fatal("short digest accepted")
	}
	if _, err := ParseHex("sha256", strings.Repeat("zz", 32)); err == nil {
		t.Fatal("non-hex digest accepted")
	}
	if _, err := ParseHex("crc", want); err == nil {
		t.Fatal("unknown algorithm accepted")
	}
}

func TestParseSRI(t *testing.T) {
	raw := sha256.Sum256([]byte("payload"))
	value := "sha256-" + base64.StdEncoding.EncodeToString(raw[:])
	got, err := ParseSRI(value)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Algo != "sha256" || got[0].Hex != hex.EncodeToString(raw[:]) {
		t.Fatalf("digests = %#v", got)
	}
	if _, err := ParseSRI(""); err == nil {
		t.Fatal("empty SRI accepted")
	}
	if _, err := ParseSRI("sha256-not-base64!"); err == nil {
		t.Fatal("invalid base64 accepted")
	}
	if _, err := ParseSRI("crc-abc"); err == nil {
		t.Fatal("unknown SRI algorithm accepted")
	}
}

func TestHasherAndCheck(t *testing.T) {
	raw := []byte("hash me")
	h, err := NewHasher("sha256", "sha512", "sha256")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Write(raw); err != nil {
		t.Fatal(err)
	}
	digests := h.Sum()
	if len(digests) != 2 {
		t.Fatalf("digests = %#v", digests)
	}
	sha := sha256.Sum256(raw)
	if digests["sha256"] != hex.EncodeToString(sha[:]) {
		t.Fatalf("sha256 = %s", digests["sha256"])
	}
	if err := Check([]Digest{{Algo: "sha256", Hex: digests["sha256"]}}, digests); err != nil {
		t.Fatal(err)
	}
	err = Check([]Digest{{Algo: "sha256", Hex: strings.Repeat("00", 32)}}, digests)
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("mismatch error = %v", err)
	}
	var mismatch *MismatchError
	if !errors.As(err, &mismatch) || mismatch.Got != digests["sha256"] {
		t.Fatalf("mismatch detail = %#v", mismatch)
	}
	if err := Check([]Digest{{Algo: "md5", Hex: strings.Repeat("00", 16)}}, digests); !errors.Is(err, ErrMismatch) {
		t.Fatalf("missing computed digest error = %v", err)
	}
	if _, err := NewHasher("crc"); err == nil {
		t.Fatal("unknown hasher algorithm accepted")
	}
}

func FuzzParseHex(f *testing.F) {
	for _, seed := range []string{"", strings.Repeat("ab", 32), "zz", "abc"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		_, _ = ParseHex("sha256", value)
	})
}

func FuzzParseSRI(f *testing.F) {
	f.Add("")
	f.Add("sha256-abc")
	f.Add("sha512-")
	f.Fuzz(func(t *testing.T, value string) {
		_, _ = ParseSRI(value)
	})
}
