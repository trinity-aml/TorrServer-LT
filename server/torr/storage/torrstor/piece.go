package torrstor

import (
	"sync"
	"sync/atomic"
	"time"

	"server/settings"
)

// pieceBlockSize is libtorrent's wire block (default_block_size, 16 KiB): every
// disk write lands as one or more whole blocks at block-aligned offsets. The
// availability bitmap below tracks the piece at this granularity.
const pieceBlockSize = 16 << 10

// Piece is a single torrent piece. Persistence picks between MemPiece
// (RAM) and DiskPiece (file-per-piece on disk) according to BTsets.
type Piece struct {
	cache *Cache
	Id    int

	mu   sync.RWMutex
	mem  *MemPiece
	disk *DiskPiece
	size int64
	// avail is the written-block bitmap (one bit per pieceBlockSize block, lazily
	// allocated on first write). It is what lets a Reader serve bytes from an
	// INCOMPLETE piece the moment the blocks under the read offset land — the
	// upstream anacrolix SetResponsive() behaviour — instead of parking until the
	// whole piece downloads AND hash-verifies (a full-piece + hash wait on every
	// seek and at the wire edge after a preload: the "pause" both felt like).
	// p.size can't do this job: it is only the highest written offset and says
	// nothing about holes below it.
	avail []uint64
	// accessed is the last read/write unix time; atomic so ReadAt can stamp it
	// under the shared (read) lock without racing concurrent readers of the same
	// piece, and so Accessed() can be read lock-free for LRU eviction sorting.
	accessed atomic.Int64
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
	// Load the atomically-swapped pointer once: separate BTsets() calls can race
	// a runtime swap (or a test resetting it to nil) and nil-panic mid-expression.
	s := settings.BTsets()
	return s != nil && s.UseDisk && s.TorrentsSavePath != ""
}

func savePath() string {
	if s := settings.BTsets(); s != nil {
		return s.TorrentsSavePath
	}
	return ""
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
		// NOTE: do NOT mark the piece complete here. p.size is only the highest
		// written offset; blocks arrive out of order, so the last block (by
		// offset) often lands first and would set size>=expected while the piece
		// still has holes — a reader would then read garbage/zeros. Completion is
		// driven solely by libtorrent's piece_finished_alert (all blocks present
		// AND hash-verified) via Cache.SignalPieceComplete -> setComplete.
		p.markAvailLocked(off, int64(n))
		p.accessed.Store(time.Now().Unix())
	}
	return n, err
}

// markAvailLocked flags the 16 KiB blocks FULLY covered by the write [off,
// off+n) as available. Called with p.mu held (from WriteAt). Only whole blocks
// count: a short write into the torrent's very last piece (whose real length is
// below PieceLength, which we don't know here) leaves its tail block unmarked,
// so reads there simply fall back to waiting for the hash-verified complete
// flag — one piece per torrent, always preloaded/pinned anyway (EOF index).
func (p *Piece) markAvailLocked(off, n int64) {
	plen := p.cache.PieceLength
	if plen <= 0 || n <= 0 {
		return
	}
	nb := int((plen + pieceBlockSize - 1) / pieceBlockSize)
	if p.avail == nil {
		p.avail = make([]uint64, (nb+63)/64)
	}
	first := int((off + pieceBlockSize - 1) / pieceBlockSize) // first block starting at/after off
	end := off + n
	last := int(end / pieceBlockSize) // exclusive: blocks fully covered
	if end >= plen {
		last = nb // a write reaching the piece end covers its (possibly short) tail block
	}
	for b := first; b < last && b < nb; b++ {
		p.avail[b>>6] |= 1 << uint(b&63)
	}
}

// availableFrom reports how many bytes are contiguously readable starting at
// off: the whole remainder for a complete piece, else the run of consecutive
// written blocks from off (0 when the block under off hasn't arrived). This is
// the responsive-read predicate: Reader serves exactly this prefix without
// waiting for the rest of the piece or its hash.
func (p *Piece) availableFrom(off int64) int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	plen := p.cache.PieceLength
	if plen <= 0 || off < 0 || off >= plen {
		return 0
	}
	if p.complete {
		// Complete but with the backing data already released (close path) serves
		// nothing — mirrors ReadAt returning EOF there.
		if p.disk == nil && (p.mem == nil || p.mem.buf == nil) {
			return 0
		}
		return plen - off
	}
	if p.avail == nil {
		return 0
	}
	nb := int((plen + pieceBlockSize - 1) / pieceBlockSize)
	b := int(off / pieceBlockSize)
	for b < nb && p.avail[b>>6]&(1<<uint(b&63)) != 0 {
		b++
	}
	end := int64(b) * pieceBlockSize
	if end > plen {
		end = plen
	}
	if end <= off {
		return 0
	}
	return end - off
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
		p.accessed.Store(time.Now().Unix())
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
	p.avail = nil // the in-memory blocks are gone; don't advertise them as readable
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
	p.avail = nil
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
	return p.accessed.Load()
}
