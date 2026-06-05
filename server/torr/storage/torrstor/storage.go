// Package torrstor is the Go-side piece cache backing libtorrent's custom
// disk_interface (defined in server/lt). It owns one Cache per running
// torrent and routes the disk callbacks emitted by libtorrent's disk
// threads to the right per-torrent Cache.
//
// Etap 4.1 — in-memory only: every Piece's bytes live in a MemPiece.
// Etap 4.2 — adds DiskPiece (TorrentsSavePath/<hash>/<id>) plus the resume
// scan that populates the have-bitmap from existing piece files.
package torrstor

import (
	"errors"
	"io"
	"sync"

	"server/lt"
)

// Storage is the process-wide registry of per-torrent caches. Plug it
// into libtorrent via Install(); calls from libtorrent's disk threads
// land on its Read/Write/Open/Close/Deleted/Have methods.
type Storage struct {
	mu     sync.RWMutex
	caches map[int64]*Cache    // by libtorrent storage_id
	byHash map[[20]byte]*Cache // by info hash for Reader lookup from torr
}

// NewStorage constructs an empty registry.
func NewStorage() *Storage {
	return &Storage{
		caches: map[int64]*Cache{},
		byHash: map[[20]byte]*Cache{},
	}
}

var (
	globalOnce sync.Once
	globalSt   *Storage
)

// Global returns the process-wide Storage singleton (constructed on first
// access). BTServer.Connect plumbs it into libtorrent via Install().
func Global() *Storage {
	globalOnce.Do(func() { globalSt = NewStorage() })
	return globalSt
}

// Install registers s as the disk_io backend for the next session_new.
// Already-running sessions keep their original disk_io.
func (s *Storage) Install() error {
	return lt.RegisterStorageCallbacks(lt.StorageCallbacks{
		Open:    s.callbackOpen,
		Close:   s.callbackClose,
		Deleted: s.callbackDeleted,
		Read:    s.callbackRead,
		Write:   s.callbackWrite,
		Have:    s.callbackHave,
	})
}

// Uninstall reverts to libtorrent's default disk_io on the next session_new.
func (s *Storage) Uninstall() error {
	return lt.RegisterStorageCallbacks(lt.StorageCallbacks{})
}

// CacheByHash returns the live Cache for the given v1 info hash, or nil
// if no torrent with that hash is currently registered.
func (s *Storage) CacheByHash(hash [20]byte) *Cache {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byHash[hash]
}

// ----- lt.StorageCallbacks dispatch -----

func (s *Storage) callbackOpen(storage int64, hash [20]byte, numPieces int, pieceLength int64) {
	c := newCache(s, storage, hash, numPieces, pieceLength)
	s.mu.Lock()
	s.caches[storage] = c
	s.byHash[hash] = c
	s.mu.Unlock()
	// In UseDisk mode, eagerly materialise any pre-existing piece
	// files so Have()/Reader hit them without going through the lazy
	// reconstruction path in readPiece.
	c.scanLocalPieces()
}

func (s *Storage) callbackClose(storage int64) {
	s.mu.Lock()
	c := s.caches[storage]
	if c != nil {
		delete(s.byHash, c.InfoHash)
		delete(s.caches, storage)
	}
	s.mu.Unlock()
	if c != nil {
		c.close()
	}
}

func (s *Storage) callbackDeleted(storage int64) {
	s.mu.RLock()
	c := s.caches[storage]
	s.mu.RUnlock()
	if c != nil {
		c.wipe()
	}
}

func (s *Storage) callbackRead(storage int64, piece int, offset int64, dst []byte) (int, error) {
	c := s.lookup(storage)
	if c == nil {
		return 0, errStorageMissing
	}
	return c.readPiece(piece, offset, dst)
}

func (s *Storage) callbackWrite(storage int64, piece int, offset int64, src []byte) (int, error) {
	c := s.lookup(storage)
	if c == nil {
		return 0, errStorageMissing
	}
	return c.writePiece(piece, offset, src)
}

func (s *Storage) callbackHave(storage int64, piece int) bool {
	c := s.lookup(storage)
	if c == nil {
		return false
	}
	return c.Have(piece)
}

func (s *Storage) lookup(storage int64) *Cache {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.caches[storage]
}

// ----- errors -----

var (
	errStorageMissing = errors.New("torrstor: no cache for storage_id")
	errOutOfPiece     = errors.New("torrstor: offset beyond piece length")
	_                 = io.EOF // keep import even when only used indirectly
)
