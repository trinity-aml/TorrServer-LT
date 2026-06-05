package torrstor

import (
	"sync"
	"time"

	"server/settings"
)

// Piece is a single torrent piece. Persistence picks between MemPiece
// (RAM) and DiskPiece (file-per-piece on disk) according to BTsets.
type Piece struct {
	cache *Cache
	Id    int

	mu       sync.RWMutex
	mem      *MemPiece
	disk     *DiskPiece
	size     int64
	accessed int64
	complete bool
}

func newPiece(c *Cache, id int) *Piece {
	p := &Piece{cache: c, Id: id}
	if useDisk() {
		p.disk = newDiskPiece(p, savePath())
	} else {
		p.mem = newMemPiece(p)
	}
	return p
}

// useDisk reflects the current settings choice. Checked per Piece so
// re-opening a Cache after a settings change picks the new backend
// (existing Pieces keep their original backend until released).
func useDisk() bool {
	return settings.BTsets != nil && settings.BTsets.UseDisk &&
		settings.BTsets.TorrentsSavePath != ""
}

func savePath() string {
	if settings.BTsets == nil {
		return ""
	}
	return settings.BTsets.TorrentsSavePath
}

func (p *Piece) WriteAt(b []byte, off int64) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var (
		n   int
		err error
	)
	if p.disk != nil {
		n, err = p.disk.WriteAt(b, off)
	} else if p.mem != nil {
		n, err = p.mem.WriteAt(b, off)
	}
	if n > 0 {
		end := off + int64(n)
		if end > p.size {
			p.size = end
		}
		if p.size >= p.expectedSize() {
			p.complete = true
		}
		p.accessed = time.Now().Unix()
	}
	return n, err
}

func (p *Piece) ReadAt(b []byte, off int64) (int, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var (
		n   int
		err error
	)
	if p.disk != nil {
		n, err = p.disk.ReadAt(b, off)
	} else if p.mem != nil {
		n, err = p.mem.ReadAt(b, off)
	}
	if n > 0 {
		p.accessed = time.Now().Unix()
	}
	return n, err
}

// expectedSize accounts for the final piece being potentially shorter
// than PieceLength.
func (p *Piece) expectedSize() int64 {
	return p.cache.PieceLength
}

// release frees the in-memory buffer only. The on-disk file (if any)
// is preserved so a subsequent Cache.Open on the same hash can resume
// without re-downloading. Use wipe() to remove the disk file too.
func (p *Piece) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mem != nil {
		p.mem.Release()
	}
}

// wipe drops in-memory state AND removes the on-disk file. Used by
// Cache.wipe() (libtorrent requested file deletion) and by the LRU
// eviction path (the cache is over capacity and we need disk space).
func (p *Piece) wipe() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disk != nil {
		p.disk.Release()
	}
	if p.mem != nil {
		p.mem.Release()
	}
	p.size = 0
	p.complete = false
}

func (p *Piece) SizeBytes() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.size
}

func (p *Piece) Complete() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.complete
}

func (p *Piece) setComplete(v bool) {
	p.mu.Lock()
	p.complete = v
	p.mu.Unlock()
}

func (p *Piece) Accessed() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.accessed
}
