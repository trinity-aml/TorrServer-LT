// Package torr is the Go-side BitTorrent engine adapter.
//
// Etap 3 swap-in: the underlying engine is libtorrent (arvidn) via server/lt,
// replacing the former anacrolix/torrent backend. Public surface kept stable
// for the rest of the project (settings/web/dlna/tgbot etc.) — Hash and
// TorrentSpec match the on-disk JSON schema produced by the previous
// release, so existing config.db files load without migration.
package torr

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
)

// Hash is the v1 SHA-1 BitTorrent info hash. JSON-encoded as a 40-char
// lowercase hex string for backward compatibility with anacrolix's
// metainfo.Hash on-disk format.
type Hash [20]byte

// HexString returns the lowercase hex form (40 chars).
func (h Hash) HexString() string { return hex.EncodeToString(h[:]) }

// String is an alias for HexString.
func (h Hash) String() string { return h.HexString() }

// IsZero reports whether the hash is all-zero.
func (h Hash) IsZero() bool {
	for _, b := range h {
		if b != 0 {
			return false
		}
	}
	return true
}

// MarshalText / UnmarshalText drive both encoding/json and encoding/text
// users; this matches metainfo.Hash semantics.
func (h Hash) MarshalText() ([]byte, error) { return []byte(h.HexString()), nil }

func (h *Hash) UnmarshalText(b []byte) error {
	if len(b) != 40 {
		return fmt.Errorf("torr.Hash: bad hex length %d (want 40)", len(b))
	}
	dec, err := hex.DecodeString(string(bytes.ToLower(b)))
	if err != nil {
		return fmt.Errorf("torr.Hash: %w", err)
	}
	copy(h[:], dec)
	return nil
}

// NewHashFromHex parses a 40-char hex string into a Hash. Returns zero
// Hash if the input is malformed.
func NewHashFromHex(s string) Hash {
	var h Hash
	_ = h.UnmarshalText([]byte(s))
	return h
}

// MustParseHash is the panicking variant of NewHashFromHex.
func MustParseHash(s string) Hash {
	var h Hash
	if err := h.UnmarshalText([]byte(s)); err != nil {
		panic(err)
	}
	return h
}

// TorrentSpec mirrors the on-disk JSON schema of anacrolix's
// torrent.TorrentSpec (the relevant subset). Field names and tags are
// preserved so existing config.db Torrents bucket loads unchanged.
type TorrentSpec struct {
	InfoHash    Hash       `json:"InfoHash"`
	InfoBytes   []byte     `json:"InfoBytes,omitempty"`
	Trackers    [][]string `json:"Trackers,omitempty"`
	DisplayName string     `json:"DisplayName,omitempty"`
}

// FlatTrackers returns the merged single-tier list (the layout libtorrent
// expects for trackers_csv).
func (s *TorrentSpec) FlatTrackers() []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, tier := range s.Trackers {
		out = append(out, tier...)
	}
	return out
}

// File is the user-visible per-file description, decoupled from any engine.
type File struct {
	Index  int
	Path   string
	Length int64
	Offset int64
}

// DisplayPath returns the path as-is (kept for API parity with the old
// torrent.File.DisplayPath()).
func (f *File) DisplayPath() string {
	if f == nil {
		return ""
	}
	return f.Path
}

// Piece priorities (mirror libtorrent's download_priority_t). The numeric
// values follow the libtorrent convention, not anacrolix's old names:
//
//	0  = don't download
//	1  = low
//	4  = normal (default)
//	6  = high
//	7  = top priority (i.e. NOW)
type Priority int

const (
	PriorityNone     Priority = 0
	PriorityLow      Priority = 1
	PriorityNormal   Priority = 4
	PriorityHigh     Priority = 6
	PriorityReadahead Priority = 5
	PriorityNext     Priority = 6
	PriorityNow      Priority = 7
)

// ErrNotImplemented is returned by streaming/storage code paths that
// have not been wired up to libtorrent yet (Etap 4/5).
var ErrNotImplemented = errors.New("torr: not implemented in this milestone")
