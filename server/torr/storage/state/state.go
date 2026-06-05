// Package state holds the engine-agnostic DTOs for the piece cache.
// Until Etap 4 wires the real custom storage these structs are still
// emitted by torr.Torrent.CacheState() so that downstream consumers
// (/cache HTTP endpoint, tgbot snake command) keep their typed schema.
package state

import (
	"server/torr/state"
)

// CacheState is the snapshot served by /cache and friends. Field set is
// preserved from the pre-libtorrent code so JSON callers don't break.
type CacheState struct {
	Hash         string
	Capacity     int64
	Filled       int64
	PiecesLength int64
	PiecesCount  int
	Torrent      *state.TorrentStatus
	Pieces       map[int]ItemState
	Readers      []*ReaderState
}

type ItemState struct {
	Id        int
	Length    int64
	Size      int64
	Completed bool
	Priority  int
}

type ReaderState struct {
	Start  int
	End    int
	Reader int
}
