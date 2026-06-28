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
	// Strict model: the cache budget is the sliding window (behind + current + ahead)
	// PLUS the EOF seek-index tail pin held outside it. With CacheSize 10 pieces and
	// ReadAhead 80% the tail pin is the 5 MB EOF index, which on this tiny 16 KB-piece
	// file is capped to maxTailPinPieces = 6 and bound to the last 6, so the window
	// budget is 10-6 = 4: behind = round(4*20%) = 1, ahead = 4-1-1 = 2, so at cur=20 the
	// window is exactly [19..22], the tail index [34..39] stays pinned, and everything
	// else (including the former head pin [0..1]) is dropped.
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
	// The whole [19..22] window survives — behind margin + current + forward readahead
	// — plus the pinned EOF seek index [34..39].
	for _, keep := range []int{19, 20, 22, 34, 39} {
		if !present(keep) {
			t.Fatalf("protected piece %d was evicted", keep)
		}
	}
	// Everything outside the window is dropped, even old ones — INCLUDING the former
	// head pin [0..1]. The tail index [34..39] now survives (pinned the whole stream).
	for _, gone := range []int{0, 1, 2, 10, 17, 18, 23, 25, 33} {
		if present(gone) {
			t.Fatalf("unprotected piece %d should have been evicted", gone)
		}
	}
	if c.Filled() > c.capacity() {
		t.Fatalf("eviction did not converge: filled=%d cap=%d", c.Filled(), c.capacity())
	}
}

// TestCache_EvictionSparesBothDeviceWindows is the multi-device case: two
// devices (distinct groups) stream the same torrent from far-apart positions.
// Each group must get its OWN protected sliding window — neither device's
// just-about-to-play pieces may be evicted to make room for the other's. With
// CacheSize 10 pieces / ReadAhead 80% the tail pin is capped to 6 pieces, so the
// window budget is 10-6 = 4 (behind 1 + current + ahead 2): device A at piece 20
// protects [19..22] and device B at piece 60 protects [59..62], plus the shared EOF
// seek index [74..79]; capacity grows (streamingReserve) to fit both, and everything
// outside is dropped.
func TestCache_EvictionSparesBothDeviceWindows(t *testing.T) {
	prev := settings.BTsets()
	settings.StoreBTsets(&settings.BTSets{UseDisk: false, CacheSize: 10 * pieceLen, ReaderReadAHead: 80})
	t.Cleanup(func() { settings.StoreBTsets(prev) })

	s := NewStorage()
	h := mkHash(0x3C)
	const total = 80
	s.callbackOpen(1, h, total, pieceLen)
	c := s.CacheByHash(h)

	payload := bytes.Repeat([]byte{0x9}, int(pieceLen))
	c.mu.Lock()
	for i := 0; i < total; i++ {
		p := newPiece(c, i)
		if _, err := p.WriteAt(payload, 0); err != nil {
			c.mu.Unlock()
			t.Fatalf("write %d: %v", i, err)
		}
		p.setComplete(true)
		p.accessed.Store(int64(i)) // all old, so only protection keeps a piece
		c.pieces[i] = p
	}
	c.mu.Unlock()

	// Device A streams piece 20; device B streams piece 60 — different groups.
	rA := &Reader{cache: c, group: "deviceA", file: FileInfo{Offset: 0, Length: total * pieceLen}}
	rA.readahead.Store(8 * pieceLen)
	rA.offset.Store(20 * pieceLen)
	rA.winFirst.Store(20)
	rA.winLast.Store(28)
	c.registerReader(rA)

	rB := &Reader{cache: c, group: "deviceB", file: FileInfo{Offset: 0, Length: total * pieceLen}}
	rB.readahead.Store(8 * pieceLen)
	rB.offset.Store(60 * pieceLen)
	rB.winFirst.Store(60)
	rB.winLast.Store(68)
	c.registerReader(rB)

	c.evictIfOverCapacity()

	present := func(id int) bool {
		c.mu.RLock()
		defer c.mu.RUnlock()
		return c.pieces[id] != nil
	}
	// BOTH device windows survive: [19..22] (A) and [59..62] (B), plus the pinned EOF
	// seek index [74..79].
	for _, keep := range []int{19, 20, 22, 59, 60, 62, 74, 79} {
		if !present(keep) {
			t.Fatalf("protected piece %d was evicted (device isolation broken)", keep)
		}
	}
	// Pieces well outside both windows are dropped — including the former head pin
	// [0..1]. The tail index [74..79] now survives (pinned the whole stream).
	for _, gone := range []int{0, 1, 10, 18, 23, 40, 50, 55, 58, 63, 73} {
		if present(gone) {
			t.Fatalf("unprotected piece %d should have been evicted", gone)
		}
	}
}

// TestCache_TailPinSurvivesPreloadHandoff reproduces the AVI idx1 stall from the
// field log (Cherniy.dvor ...AVI): the EOF seek index lives in the last bytes of the
// file, which the preload buffered as tail pieces 144..145 — a BYTE range that
// STRADDLES the 144/145 boundary. After the head reader advances to piece 1 and the
// preload reserve is handed off, the capacity fallback (filled 72 MB > 64 MB cache)
// must NOT evict piece 144: the standing per-reader tail pin (tailReserve) keeps the
// WHOLE idx1 resident so the player's EOF probe reads it and playback starts. Before
// the straddle fix the pin covered only piece 145, so 144 was evicted, REFETCHed, and
// the stream never started ("после прелоада стрим не стартовал").
func TestCache_TailPinSurvivesPreloadHandoff(t *testing.T) {
	const MB = int64(1) << 20
	plen := 4 * MB
	prev := settings.BTsets()
	settings.StoreBTsets(&settings.BTSets{UseDisk: false, CacheSize: 64 * MB, ReaderReadAHead: 95})
	t.Cleanup(func() { settings.StoreBTsets(prev) })

	s := NewStorage()
	h := mkHash(0xA1)
	const numPieces = 146
	// File ends 2 MB into the last piece (145), so the 4 MB tail window straddles down
	// into piece 144 — exactly the idx1 layout from the log (tail 144..145).
	fileLen := int64(145)*plen + 2*MB
	s.callbackOpen(1, h, numPieces, plen)
	c := s.CacheByHash(h)

	// Preload state: head 0..15 + tail 144..145 resident == filled 72 MB (> 64 MB cache).
	// Write one byte at plen-1 so SizeBytes reports a full 4 MB piece without the alloc.
	present := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 144, 145}
	c.mu.Lock()
	for n, i := range present {
		p := newPiece(c, i)
		if _, err := p.WriteAt([]byte{0x9}, plen-1); err != nil {
			c.mu.Unlock()
			t.Fatalf("write %d: %v", i, err)
		}
		p.setComplete(true)
		p.accessed.Store(int64(n)) // head pieces are oldest (LRU drops them first)
		c.pieces[i] = p
	}
	c.mu.Unlock()

	// Preload reserve exactly as torr/preload.go sets it: head window + tail window.
	c.SetPreloadReserve([][2]int{{0, 15}, {144, 145}})

	// Head reader advanced to piece 1 — window established (non-probe).
	rHead := &Reader{cache: c, group: "dev", file: FileInfo{Offset: 0, Length: fileLen}}
	rHead.readahead.Store(60 * MB)
	rHead.offset.Store(1 * plen)
	rHead.winFirst.Store(0)
	rHead.winLast.Store(13)
	rHead.lastRead.Store(time.Now().Unix())
	c.registerReader(rHead)

	// EOF index probe: a second connection reading the idx1 at piece 144 (no window).
	rTail := &Reader{cache: c, group: "dev", file: FileInfo{Offset: 0, Length: fileLen}}
	rTail.readahead.Store(60 * MB)
	rTail.offset.Store(144 * plen)
	rTail.winFirst.Store(-1)
	rTail.winLast.Store(-1)
	rTail.lastRead.Store(time.Now().Unix())
	c.registerReader(rTail)

	// Head reader hands the preload reservation over to its window (as scheduleWindow
	// does once cur advances past the head's first piece): the tail must stay protected
	// by the standing tail pin, NOT by the now-cleared preload reserve.
	c.ClearPreloadReserve()

	c.evictIfOverCapacity()

	got := func(id int) bool {
		c.mu.RLock()
		defer c.mu.RUnlock()
		return c.pieces[id] != nil
	}
	// The idx1 tail — BOTH 144 and 145 — must survive the handoff. This is the fix.
	for _, keep := range []int{144, 145} {
		if !got(keep) {
			t.Fatalf("EOF index piece %d evicted after preload handoff (idx1 stall)", keep)
		}
	}
	// And it converged to the budget by dropping head leftovers past the window.
	if c.Filled() > c.capacity() {
		t.Fatalf("eviction did not converge: filled=%d cap=%d", c.Filled(), c.capacity())
	}
}

// TestCache_NoProactiveSweepKeepsLruBuffer pins the other half of the post-seek fix:
// eviction is capacity-driven LRU (the original TorrServer model), NOT a proactive
// sweep that reaps every piece outside the window the moment the anchor passes it.
// The abandoned region a forward seek leaves behind must LINGER as the oldest pieces
// (the sacrificial buffer) while the cache is UNDER capacity — so that when readahead
// later needs room, those stale pieces are sacrificed first and the snake's recently
// played pieces survive. The old proactive sweep dropped the buffer at once, leaving
// only the just-read pieces to evict under the next bit of pressure → refetch.
func TestCache_NoProactiveSweepKeepsLruBuffer(t *testing.T) {
	const MB = int64(1) << 20
	plen := 4 * MB
	prev := settings.BTsets()
	settings.StoreBTsets(&settings.BTSets{UseDisk: false, CacheSize: 64 * MB, ReaderReadAHead: 95})
	t.Cleanup(func() { settings.StoreBTsets(prev) })

	s := NewStorage()
	h := mkHash(0xE3)
	const numPieces = 150
	s.callbackOpen(1, h, numPieces, plen)
	c := s.CacheByHash(h)
	fileLen := int64(numPieces) * plen

	// Resident: a small abandoned old region 0..4 (5 pieces) + the live window region
	// 56..63 (8 pieces) = 13 pieces = 52 MB, UNDER the 64 MB cache. Nothing must be
	// evicted: there is room, and the old region is the LRU buffer for later.
	now := time.Now().Unix()
	c.mu.Lock()
	for _, i := range []int{0, 1, 2, 3, 4} {
		p := newPiece(c, i)
		_, _ = p.WriteAt([]byte{0x9}, plen-1)
		p.setComplete(true)
		p.accessed.Store(now - 100) // old, but UNDER capacity so it must stay
		c.pieces[i] = p
	}
	for i := 56; i <= 63; i++ {
		p := newPiece(c, i)
		_, _ = p.WriteAt([]byte{0x9}, plen-1)
		p.setComplete(true)
		p.accessed.Store(now)
		c.pieces[i] = p
	}
	c.mu.Unlock()

	r := &Reader{cache: c, group: "dev", file: FileInfo{Offset: 0, Length: fileLen}}
	r.readahead.Store(60 * MB)
	r.offset.Store(60 * plen)
	r.winFirst.Store(56)
	r.winLast.Store(68)
	r.lastRead.Store(now)
	c.registerReader(r)

	c.evictIfOverCapacity()

	present := func(id int) bool {
		c.mu.RLock()
		defer c.mu.RUnlock()
		return c.pieces[id] != nil
	}
	// The abandoned old region survives: under capacity, the LRU buffer is NOT reaped.
	for _, keep := range []int{0, 1, 2, 3, 4} {
		if !present(keep) {
			t.Fatalf("old piece %d proactively reaped under capacity (sacrificial buffer lost)", keep)
		}
	}
}

// TestCache_LargePieceCapacityBounded reproduces the field report: a 64 MB cache
// on a torrent with 16 MB pieces ballooned a SINGLE viewer's capacity to 192 MB
// (window 7 pieces + head pin 2 + tail pin 3, all disjoint for a mid-file
// playhead). The window must be capped to the cache budget and the pins
// byte-bounded, so one viewer stays ~CacheSize + a small pin overhang.
func TestCache_LargePieceCapacityBounded(t *testing.T) {
	const MB = int64(1) << 20
	prev := settings.BTsets()
	settings.StoreBTsets(&settings.BTSets{UseDisk: false, CacheSize: 64 * MB, ReaderReadAHead: 95})
	t.Cleanup(func() { settings.StoreBTsets(prev) })

	plen := 16 * MB
	s := NewStorage()
	h := mkHash(0x2B)
	const numPieces = 128 // a 2 GB file, offset 0
	s.callbackOpen(1, h, numPieces, plen)
	c := s.CacheByHash(h)

	// The window itself must never exceed the configured cache budget (4 pieces).
	behind, ahead := c.readerWindowPieces()
	if window := behind + ahead + 1; int64(window)*plen > 64*MB {
		t.Fatalf("window %d pieces (%d MB) exceeds the 64 MB cache budget", window, int64(window)*plen/MB)
	}

	// One viewer playing mid-file at piece 60, window established.
	r := &Reader{cache: c, group: "dev", file: FileInfo{Offset: 0, Length: int64(numPieces) * plen}}
	r.readahead.Store(60 * MB)
	r.offset.Store(60 * plen)
	r.winFirst.Store(int64(60 - behind))
	r.winLast.Store(int64(60 + ahead))
	c.registerReader(r)

	// Was 192 MB; the cap + byte-bounded pins bring it to ~CacheSize + the two
	// single-piece pins. Assert it is comfortably under the old 3x blowup.
	cap := c.capacity()
	if cap > 112*MB {
		t.Fatalf("single-viewer capacity %d MB too high (regression: window/pins not bounded)", cap/MB)
	}
	if cap < 64*MB {
		t.Fatalf("capacity %d MB below the configured cache", cap/MB)
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

// TestTailPiecesFor pins the field spec for the EOF-index tail pin: ONE piece when
// pieces exceed 5 MB, else enough pieces to cover 5 MB. Capped by maxTailPinPieces so
// a pathologically small piece can't pin most of the cache. The preload and the
// streaming pin both derive from this, so they buffer/hold exactly the same tail.
func TestTailPiecesFor(t *testing.T) {
	const MB = int64(1) << 20
	cases := []struct {
		plen int64
		want int
	}{
		{8 * MB, 1},  // > 5 MB -> one whole chunk
		{6 * MB, 1},  // > 5 MB -> one whole chunk
		{5 * MB, 1},  // == 5 MB -> the chunk already covers it
		{4 * MB, 2},  // < 5 MB -> ceil(5/4) = 2
		{2 * MB, 3},  // < 5 MB -> ceil(5/2) = 3
		{1 * MB, 5},  // < 5 MB -> ceil(5/1) = 5
		{16 * 1024, maxTailPinPieces}, // tiny piece -> capped
		{0, 1},       // no metadata yet
	}
	for _, c := range cases {
		if got := TailPiecesFor(c.plen); got != c.want {
			t.Errorf("TailPiecesFor(%d MB) = %d, want %d", c.plen/MB, got, c.want)
		}
	}
}

// TestTailReserve_PadTailPartial verifies the optional PadTailPartial setting: when the
// file's last piece is a SHORT partial (smaller than a full piece AND under 5 MB), the
// tail pin gains one EXTRA piece so the cache fills the budget a short final piece would
// otherwise leave short. Off by default (one piece for >5 MB pieces); on, it is two.
func TestTailReserve_PadTailPartial(t *testing.T) {
	const MB = int64(1) << 20
	plen := 8 * MB
	prev := settings.BTsets()
	t.Cleanup(func() { settings.StoreBTsets(prev) })

	s := NewStorage()
	h := mkHash(0xF1)
	// File ends 1 MB into the last piece (582): pieces 0..582, last piece = 1 MB partial.
	const numPieces = 583
	fileLen := int64(582)*plen + 1*MB
	s.callbackOpen(1, h, numPieces, plen)
	c := s.CacheByHash(h)
	r := &Reader{cache: c, file: FileInfo{Offset: 0, Length: fileLen}}

	// Off (default): tail is the single 1 MB last piece [582..582].
	settings.StoreBTsets(&settings.BTSets{UseDisk: false, CacheSize: 64 * MB, ReaderReadAHead: 95, PadTailPartial: false})
	if tr := r.tailReserve(); tr != [2]int{582, 582} {
		t.Fatalf("PadTailPartial off: tailReserve = %v, want [582 582]", tr)
	}

	// On: a short partial last piece pulls in one more piece [581..582].
	settings.StoreBTsets(&settings.BTSets{UseDisk: false, CacheSize: 64 * MB, ReaderReadAHead: 95, PadTailPartial: true})
	if tr := r.tailReserve(); tr != [2]int{581, 582} {
		t.Fatalf("PadTailPartial on: tailReserve = %v, want [581 582]", tr)
	}

	// On but a FULL last piece (file ends exactly on the boundary): no padding.
	rFull := &Reader{cache: c, file: FileInfo{Offset: 0, Length: int64(numPieces) * plen}}
	if tr := rFull.tailReserve(); tr != [2]int{582, 582} {
		t.Fatalf("PadTailPartial on, full last piece: tailReserve = %v, want [582 582]", tr)
	}
}
