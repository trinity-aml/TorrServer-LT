package torrstor

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"server/settings"
)

// withDiskCache flips settings.BTsets into "disk cache" mode pointing
// at a fresh temporary directory for the test, restoring the previous
// configuration at the end. Returns the temp dir for inspection.
func withDiskCache(t *testing.T, cacheSize int64) string {
	t.Helper()
	prev := settings.BTsets()
	dir := t.TempDir()
	settings.StoreBTsets(&settings.BTSets{
		UseDisk:          true,
		TorrentsSavePath: dir,
		CacheSize:        cacheSize,
	})
	t.Cleanup(func() { settings.StoreBTsets(prev) })
	return dir
}

func TestDiskPiece_RoundTrip(t *testing.T) {
	dir := withDiskCache(t, 0)
	s := NewStorage()
	h := mkHash(0xA1)
	s.callbackOpen(1, h, 4, pieceLen)

	payload := bytes.Repeat([]byte{0x9F}, int(pieceLen))
	if n, err := s.callbackWrite(1, 0, 0, payload); err != nil || n != len(payload) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}

	// Piece file exists on disk under <dir>/<hash>/0
	pp := filepath.Join(dir, hashHex(h), "0")
	fi, err := os.Stat(pp)
	if err != nil {
		t.Fatalf("piece file not on disk: %v", err)
	}
	if fi.Size() != pieceLen {
		t.Fatalf("piece file size: got %d, want %d", fi.Size(), pieceLen)
	}

	// Read it back through the storage callback.
	dst := make([]byte, len(payload))
	if n, err := s.callbackRead(1, 0, 0, dst); err != nil || n != len(payload) {
		t.Fatalf("read: n=%d err=%v", n, err)
	}
	if !bytes.Equal(dst, payload) {
		t.Fatal("disk piece content mismatch")
	}
}

func TestDiskPiece_SurvivesCacheClose(t *testing.T) {
	dir := withDiskCache(t, 0)
	hash := mkHash(0xB2)

	{
		s := NewStorage()
		s.callbackOpen(1, hash, 4, pieceLen)
		payload := bytes.Repeat([]byte{0xAB}, int(pieceLen))
		if _, err := s.callbackWrite(1, 0, 0, payload); err != nil {
			t.Fatalf("write: %v", err)
		}
		s.callbackClose(1)
		// The piece file must remain on disk after Close (only Deleted
		// wipes it).
		if _, err := os.Stat(filepath.Join(dir, hashHex(hash), "0")); err != nil {
			t.Fatalf("piece file should survive Close: %v", err)
		}
	}

	// Fresh Storage, same hash: scan should see the existing piece.
	bm := ScanHavePieces(hash, 4, pieceLen)
	if len(bm) == 0 || bm[0]&0x1 == 0 {
		t.Fatalf("ScanHavePieces missed the existing piece, bitmap=%x", bm)
	}
}

func TestDiskPiece_DeletedWipes(t *testing.T) {
	dir := withDiskCache(t, 0)
	s := NewStorage()
	h := mkHash(0xC3)
	s.callbackOpen(1, h, 4, pieceLen)
	payload := bytes.Repeat([]byte{0x33}, int(pieceLen))
	_, _ = s.callbackWrite(1, 0, 0, payload)

	s.callbackDeleted(1)

	if _, err := os.Stat(filepath.Join(dir, hashHex(h), "0")); !os.IsNotExist(err) {
		t.Fatalf("piece file should be removed after Deleted, err=%v", err)
	}
}

func TestScanHavePieces_PartialAndFinal(t *testing.T) {
	dir := withDiskCache(t, 0)
	h := mkHash(0xD4)
	root := filepath.Join(dir, hashHex(h))
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	const numPieces = 5
	// piece 0 — full
	must := func(path string, sz int64) {
		if err := os.WriteFile(path, bytes.Repeat([]byte{0x55}, int(sz)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(root, "0"), pieceLen)
	// piece 1 — short (partial) → NOT a have
	must(filepath.Join(root, "1"), pieceLen/2)
	// piece 2 — missing
	// piece 3 — full
	must(filepath.Join(root, "3"), pieceLen)
	// piece 4 (final) — any non-zero size counts as have
	must(filepath.Join(root, "4"), 1)

	bm := ScanHavePieces(h, numPieces, pieceLen)
	if len(bm) != (numPieces+7)/8 {
		t.Fatalf("bitmap size: got %d, want %d", len(bm), (numPieces+7)/8)
	}
	checkBit := func(i int, want bool) {
		got := bm[i/8]&(1<<uint(i%8)) != 0
		if got != want {
			t.Errorf("piece %d: have=%v, want %v", i, got, want)
		}
	}
	checkBit(0, true)
	checkBit(1, false)
	checkBit(2, false)
	checkBit(3, true)
	checkBit(4, true)
}

func TestScanHavePieces_OffWhenUseDiskFalse(t *testing.T) {
	prev := settings.BTsets()
	settings.StoreBTsets(&settings.BTSets{UseDisk: false})
	t.Cleanup(func() { settings.StoreBTsets(prev) })
	if bm := ScanHavePieces(mkHash(1), 4, pieceLen); bm != nil {
		t.Fatalf("expected nil bitmap when UseDisk=false, got %x", bm)
	}
}

func TestCache_LRUEvictsOldestWhenOverCapacity(t *testing.T) {
	// Capacity = 2 pieces; write 4 → expect at least 2 evictions.
	withDiskCache(t, 2*pieceLen)
	s := NewStorage()
	h := mkHash(0xE5)
	s.callbackOpen(1, h, 8, pieceLen)

	payload := func(b byte) []byte { return bytes.Repeat([]byte{b}, int(pieceLen)) }
	c := s.CacheByHash(h)
	for i, b := range []byte{0xAA, 0xBB, 0xCC, 0xDD} {
		if _, err := s.callbackWrite(1, i, 0, payload(b)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		// Only hash-verified pieces are evictable (incomplete ones must stay,
		// or finishing them later corrupts the hash check) — mark each piece
		// complete as the "piece_finished" alert would.
		c.MarkComplete(i)
	}
	// The async evictions raced the MarkComplete calls; run one deterministic
	// pass now that completeness is settled.
	c.evictIfOverCapacity()

	// Eviction may also still be running in a goroutine; wait up to 2s.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Filled() <= 2*pieceLen {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c.Filled() > 2*pieceLen {
		t.Fatalf("LRU eviction did not converge: filled=%d, capacity=%d",
			c.Filled(), 2*pieceLen)
	}
}

func TestCache_EvictionSparesReaderWindow(t *testing.T) {
	// A 40-piece file is fully resident; a single reader streams from the middle.
	// Eviction must keep that reader's working set — behind margin + forward
	// window (keepBehind/keepAhead) — plus the pinned container header and tail
	// index, and drop everything else, even though some kept pieces are older
	// (lower accessed) than some dropped ones. With CacheSize 10 pieces and
	// ReadAhead 80%, a lone reader's forward window is 8 pieces and behind 2, so
	// at cur=20 it protects [18..28]; pins add [0..1] (head) and [37..39] (tail).
	prev := settings.BTsets()
	settings.StoreBTsets(&settings.BTSets{UseDisk: false, CacheSize: 10 * pieceLen, ReaderReadAHead: 80})
	t.Cleanup(func() { settings.StoreBTsets(prev) })

	s := NewStorage()
	h := mkHash(0x5A)
	const total = 40
	s.callbackOpen(1, h, total, pieceLen)
	c := s.CacheByHash(h)

	// Populate pieces directly (not via callbackWrite, which would spawn async
	// eviction). Piece i is the i-th oldest — deterministic LRU order, since
	// accessed is otherwise 1-second granular and a tight loop ties.
	payload := bytes.Repeat([]byte{0x9}, int(pieceLen))
	c.mu.Lock()
	for i := 0; i < total; i++ {
		p := newPiece(c, i)
		if _, err := p.WriteAt(payload, 0); err != nil {
			c.mu.Unlock()
			t.Fatalf("write %d: %v", i, err)
		}
		p.setComplete(true) // only hash-verified pieces are evictable
		p.accessed.Store(int64(i))
		c.pieces[i] = p
	}
	c.mu.Unlock()

	// Reader streaming the middle, at piece 20.
	r := &Reader{cache: c, file: FileInfo{Offset: 0, Length: total * pieceLen}}
	r.readahead.Store(8 * pieceLen) // 80% of the 10-piece budget
	r.offset.Store(20 * pieceLen)
	r.winFirst.Store(20) // marks the reader as actively streaming
	r.winLast.Store(28)
	c.registerReader(r)

	c.evictIfOverCapacity()

	present := func(id int) bool {
		c.mu.RLock()
		defer c.mu.RUnlock()
		return c.pieces[id] != nil
	}
	// Forward window + behind margin and the head/tail pins survive.
	for _, keep := range []int{0, 1, 18, 20, 28, 37, 39} {
		if !present(keep) {
			t.Fatalf("protected piece %d was evicted", keep)
		}
	}
	// Pieces outside the working set and the pins are dropped, even old ones.
	for _, gone := range []int{2, 10, 17, 29, 36} {
		if present(gone) {
			t.Fatalf("unprotected piece %d should have been evicted", gone)
		}
	}
	if c.Filled() > c.capacity() {
		t.Fatalf("eviction did not converge: filled=%d cap=%d", c.Filled(), c.capacity())
	}
}

func TestDiskPiece_NameLayoutMatchesLegacy(t *testing.T) {
	dir := withDiskCache(t, 0)
	s := NewStorage()
	h := mkHash(0xF6)
	s.callbackOpen(1, h, 2, pieceLen)
	_, _ = s.callbackWrite(1, 1, 0, bytes.Repeat([]byte{0x42}, int(pieceLen)))

	expected := filepath.Join(dir, hashHex(h), strconv.Itoa(1))
	if _, err := os.Stat(expected); err != nil {
		t.Fatalf("expected legacy layout %s: %v", expected, err)
	}
}
