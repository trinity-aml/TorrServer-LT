package torrstor

import (
	"sync"
	"time"
)

// Piece is a single piece. In Etap 4.1 it is RAM-backed via MemPiece;
// Etap 4.2 adds a DiskPiece alternative selected by BTSets.UseDisk.
type Piece struct {
	cache *Cache
	Id    int

	mu       sync.RWMutex
	mem      *MemPiece
	size     int64 // bytes written so far (0..PieceLength)
	accessed int64 // unix seconds of last read/write
	complete bool
}

func newPiece(c *Cache, id int) *Piece {
	p := &Piece{cache: c, Id: id}
	p.mem = newMemPiece(p)
	return p
}

func (p *Piece) WriteAt(b []byte, off int64) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, err := p.mem.WriteAt(b, off)
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
	n, err := p.mem.ReadAt(b, off)
	if n > 0 {
		p.accessed = time.Now().Unix()
	}
	return n, err
}

// expectedSize accounts for the final piece being potentially shorter
// than PieceLength.
func (p *Piece) expectedSize() int64 {
	pl := p.cache.PieceLength
	if p.Id == p.cache.NumPieces-1 && pl > 0 {
		// We don't have the actual file_storage's last-piece size here;
		// fall back to PieceLength. The exact value is observed once we
		// see EOF on writes — the last write fills less than pl bytes.
		return pl
	}
	return pl
}

// release frees the underlying buffer (Etap 4.2 also unlinks the file
// for DiskPiece).
func (p *Piece) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
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
