package torr

import (
	"encoding/base32"
	"encoding/hex"
	"strings"
	"testing"
)

func TestHashAcceptsBase32(t *testing.T) {
	const hexHash = "d8a666b76d4362ec853abab2f1554939a6e1831b"
	raw, err := hex.DecodeString(hexHash)
	if err != nil {
		t.Fatal(err)
	}
	b32 := base32.StdEncoding.EncodeToString(raw)

	// libtorrent takes a base32 btih from a magnet link, so an API lookup by the
	// same spelling has to resolve to the same torrent.
	var h Hash
	if err := h.UnmarshalText([]byte(b32)); err != nil {
		t.Fatalf("base32 %q: %v", b32, err)
	}
	if h.HexString() != hexHash {
		t.Fatalf("base32 hash = %s, want %s", h.HexString(), hexHash)
	}
	if h2 := NewHashFromHex(strings.ToLower(b32)); h2 != h {
		t.Errorf("lowercase base32 = %s, want %s", h2.HexString(), hexHash)
	}
	var bad Hash
	if err := bad.UnmarshalText([]byte("not-a-valid-base32-hash-0000000!")); err == nil {
		t.Error("a malformed 32-character string must not parse")
	}
}
