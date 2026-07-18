package torrstor

import (
	"testing"
	"time"

	"server/settings"
)

// TestStreamAnchor_HoldsTrailingConnection reproduces the window-oscillation churn
// from the field log: a per-chunk player (VLC/AVI) keeps a TRAILING playback
// connection and a LEADING read-ahead connection a few pieces apart. When the
// trailing one goes briefly idle (stale) between chunks, the anchor must NOT lurch up
// to the leading edge — doing so evicted the in-between pieces as "behind" and then
// REFETCHed them when the trailing reader read again (the 1-2 chunk GAPS the user saw
// inside the window), and extended the forward fill past the true playhead so those
// far pieces hung half-downloaded. The anchor must stay on the trailing playhead.
func TestStreamAnchor_HoldsTrailingConnection(t *testing.T) {
	const MB = int64(1) << 20
	plen := 4 * MB
	prev := settings.BTsets()
	settings.StoreBTsets(&settings.BTSets{UseDisk: false, CacheSize: 64 * MB, ReaderReadAHead: 95})
	t.Cleanup(func() { settings.StoreBTsets(prev) })

	s := NewStorage()
	h := mkHash(0xB7)
	const numPieces = 146
	s.callbackOpen(1, h, numPieces, plen)
	c := s.CacheByHash(h)
	fileLen := int64(numPieces) * plen

	now := time.Now().Unix()
	// TRAILING playback reader at piece 108 (the true playhead).
	rTrail := &Reader{cache: c, group: "dev", file: FileInfo{Offset: 0, Length: fileLen}}
	rTrail.offset.Store(108 * plen)
	rTrail.winFirst.Store(108)
	rTrail.lastRead.Store(now)
	c.registerReader(rTrail)
	// LEADING read-ahead reader at piece 112, a few pieces ahead.
	rLead := &Reader{cache: c, group: "dev", file: FileInfo{Offset: 0, Length: fileLen}}
	rLead.offset.Store(112 * plen)
	rLead.winFirst.Store(112)
	rLead.lastRead.Store(now)
	c.registerReader(rLead)

	// The anchor commits to the lowest (trailing) reader.
	if got := c.groupPlayheads()["dev"]; got != 108 {
		t.Fatalf("anchor should commit to the trailing reader 108, got %d", got)
	}

	// The trailing connection goes idle (stale) between chunks for longer than the
	// up-hold, while the leading read-ahead keeps reading. Simulate the up-hold having
	// long expired (otherwise the brief hold alone would mask the bug).
	rTrail.lastRead.Store(now - (staleReaderSec + 2)) // stale
	rLead.lastRead.Store(time.Now().Unix())           // active
	c.groupsMu.Lock()
	c.groups["dev"].playheadTime = time.Now().Unix() - (anchorHoldSec + 2)
	c.groupsMu.Unlock()

	// A trailing connection a few pieces below the anchor is NOT a seek leftover, so it
	// keeps the anchor down — no lurch to the leading edge 112.
	if got := c.groupPlayheads()["dev"]; got != 108 {
		t.Fatalf("anchor lurched off the trailing playhead to %d (oscillation); want 108", got)
	}

	// A genuine forward SEEK must still move the anchor: drop the trailing reader far
	// below the new cluster (a leftover) and put both live readers well ahead.
	rTrail.offset.Store(40 * plen) // old abandoned connection, far behind, stale
	rTrail.lastRead.Store(now - (staleReaderSec + 2))
	rLead.offset.Store(95 * plen)
	rLead.winFirst.Store(95)
	rLead.lastRead.Store(time.Now().Unix())
	c.groupsMu.Lock()
	c.groups["dev"].playheadTime = time.Now().Unix() - (anchorHoldSec + 2)
	c.groupsMu.Unlock()
	if got := c.groupPlayheads()["dev"]; got != 95 {
		t.Fatalf("forward seek: anchor should follow the live readers to 95, got %d (stuck on a leftover?)", got)
	}
}

// TestStreamAnchor_ForwardSeekReleasesLeftover guards the regression the trailing-hold
// fix could cause: after a forward seek, the OLD connection lingers stale right at the
// previous (committed) anchor. The hold must key on the lowest ACTIVE reader, not the
// committed anchor — otherwise the leftover sitting on the old anchor would pin it
// forever and the new playback position would never download (a permanent stall).
func TestStreamAnchor_ForwardSeekReleasesLeftover(t *testing.T) {
	const MB = int64(1) << 20
	plen := 4 * MB
	prev := settings.BTsets()
	settings.StoreBTsets(&settings.BTSets{UseDisk: false, CacheSize: 64 * MB, ReaderReadAHead: 95})
	t.Cleanup(func() { settings.StoreBTsets(prev) })

	s := NewStorage()
	h := mkHash(0xC9)
	const numPieces = 200
	s.callbackOpen(1, h, numPieces, plen)
	c := s.CacheByHash(h)
	fileLen := int64(numPieces) * plen

	now := time.Now().Unix()
	// Playing at piece 50 — anchor commits there.
	rOld := &Reader{cache: c, group: "dev", file: FileInfo{Offset: 0, Length: fileLen}}
	rOld.offset.Store(50 * plen)
	rOld.winFirst.Store(50)
	rOld.lastRead.Store(now)
	c.registerReader(rOld)
	if got := c.groupPlayheads()["dev"]; got != 50 {
		t.Fatalf("anchor should commit at 50, got %d", got)
	}

	// Forward seek to 100: the old connection lingers idle (stale) AT the old anchor 50,
	// a new active reader opens at 100.
	rOld.lastRead.Store(now - (staleReaderSec + 2))
	rNew := &Reader{cache: c, group: "dev", file: FileInfo{Offset: 0, Length: fileLen}}
	rNew.offset.Store(100 * plen)
	rNew.winFirst.Store(100)
	rNew.lastRead.Store(time.Now().Unix())
	c.registerReader(rNew)
	c.groupsMu.Lock()
	c.groups["dev"].playheadTime = time.Now().Unix() - (anchorHoldSec + 2)
	c.groupsMu.Unlock()

	// The leftover at 50 is a full window below the active read at 100 → abandoned, so
	// the anchor moves to 100. (Keyed on the committed anchor it would stay stuck at 50.)
	if got := c.groupPlayheads()["dev"]; got != 100 {
		t.Fatalf("forward seek stuck: anchor %d, want 100 (leftover on the old anchor pinned it)", got)
	}
}

// TestStreamAnchor_FreshFarParkSnapsAtOnce is the instant-seek rule: a fresh park
// (wait focus) farther than a window from the committed anchor commits the anchor
// immediately — no anchorHoldSec on a backward seek and no waiting out the old
// still-active connection's staleReaderSec on a forward one. Without it the OLD
// window kept its full deadline ramp for those 2-3s, splitting the swarm against
// the seek target (the field-log lag this fix removes).
func TestStreamAnchor_FreshFarParkSnapsAtOnce(t *testing.T) {
	const MB = int64(1) << 20
	plen := 4 * MB
	prev := settings.BTsets()
	settings.StoreBTsets(&settings.BTSets{UseDisk: false, CacheSize: 64 * MB, ReaderReadAHead: 95})
	t.Cleanup(func() { settings.StoreBTsets(prev) })

	s := NewStorage()
	h := mkHash(0xD4)
	const numPieces = 700
	s.callbackOpen(1, h, numPieces, plen)
	c := s.CacheByHash(h)
	fileLen := int64(numPieces) * plen
	_, aheadP := c.readerWindowPieces()

	// Playing at piece 300, connection ACTIVE (reads right now) — anchor commits there.
	r := &Reader{cache: c, group: "dev", file: FileInfo{Offset: 0, Length: fileLen}}
	r.offset.Store(300 * plen)
	r.winFirst.Store(300)
	r.lastRead.Store(time.Now().Unix())
	c.registerReader(r)
	if got := c.groupPlayheads()["dev"]; got != 300 {
		t.Fatalf("anchor should commit at 300, got %d", got)
	}

	// Forward far seek: a fresh park lands well past the window while the old
	// connection is STILL active (the player hasn't torn it down yet). The anchor
	// must snap to the park at once — not after the old connection goes stale.
	target := 300 + aheadP + 20
	c.setWaitFocus("dev", target, 0, numPieces-1, 0)
	if got := c.groupPlayheads()["dev"]; got != target {
		t.Fatalf("forward far park: anchor %d, want %d at once (old active connection held it)", got, target)
	}

	// Backward far seek: fresh park a full window below the (just-recommitted)
	// anchor must snap down immediately, skipping anchorHoldSec.
	back := target - aheadP - 30
	c.setWaitFocus("dev", back, 0, numPieces-1, 0)
	if got := c.groupPlayheads()["dev"]; got != back {
		t.Fatalf("backward far park: anchor %d, want %d at once (anchorHoldSec held it)", got, back)
	}

	// An IN-window park must NOT re-trigger the snap path (the ramp already covers
	// it): the anchor stays governed by the normal playMin machinery. The active
	// reader still sits at 300 — a full window below `back` — so the DOWN case
	// (real lower read) applies and holds/commits by the standard rules; the park
	// two pieces ahead of the anchor must not shove the anchor up onto itself.
	r.offset.Store(int64(back) * plen)
	r.lastRead.Store(time.Now().Unix())
	c.setWaitFocus("dev", back+2, 0, numPieces-1, 0)
	if got := c.groupPlayheads()["dev"]; got != back {
		t.Fatalf("in-window park moved the anchor to %d, want it held at %d", got, back)
	}
}

// TestStreamAnchor_HeadPinParkDoesNotSnap: a park inside the container-header pin
// (piece 0 re-read after eviction — VLC re-opens bytes=0- mid-play) is served by
// the blocked-force alone and must NOT pivot the whole window onto the file head.
func TestStreamAnchor_HeadPinParkDoesNotSnap(t *testing.T) {
	const MB = int64(1) << 20
	plen := 4 * MB
	prev := settings.BTsets()
	settings.StoreBTsets(&settings.BTSets{UseDisk: false, CacheSize: 64 * MB, ReaderReadAHead: 95})
	t.Cleanup(func() { settings.StoreBTsets(prev) })

	s := NewStorage()
	h := mkHash(0xE5)
	const numPieces = 700
	s.callbackOpen(1, h, numPieces, plen)
	c := s.CacheByHash(h)
	fileLen := int64(numPieces) * plen

	r := &Reader{cache: c, group: "dev", file: FileInfo{Offset: 0, Length: fileLen}}
	r.offset.Store(300 * plen)
	r.winFirst.Store(300)
	r.lastRead.Store(time.Now().Unix())
	c.registerReader(r)
	if got := c.groupPlayheads()["dev"]; got != 300 {
		t.Fatalf("anchor should commit at 300, got %d", got)
	}

	// Header re-read parks on piece 0 (inside headPinPieces): no snap.
	c.setWaitFocus("dev", 0, 0, numPieces-1, 0)
	if got := c.groupPlayheads()["dev"]; got != 300 {
		t.Fatalf("header-pin park dragged the anchor to %d, want 300", got)
	}

	// Tail-pin park (EOF index re-read after eviction): no snap either.
	c.setWaitFocus("dev", numPieces-1, 0, numPieces-1, 0)
	if got := c.groupPlayheads()["dev"]; got != 300 {
		t.Fatalf("tail-pin park dragged the anchor to %d, want 300", got)
	}
}

// TestStreamAnchor_DyingConnectionCannotStealAnchorBack reproduces the VLC
// direct-link ping-pong from the field log: at the moment of a far seek the
// player's OLD connection is blocked mid-buffer on a not-yet-complete piece, so
// right after the seek's park it parks once more at the old position — and the
// anchor flapped 183→4→5→183, holding both windows deadline-forced and
// splitting the swarm while the seek target trickled in (10.9s resume vs
// upstream's 2s). For snapShadowMs after the snap, parks AND live reads from a
// connection born before the snap near the replaced anchor must not move the
// anchor; a NEW connection seeking back there commits instantly as before.
func TestStreamAnchor_DyingConnectionCannotStealAnchorBack(t *testing.T) {
	const MB = int64(1) << 20
	plen := 4 * MB
	prev := settings.BTsets()
	settings.StoreBTsets(&settings.BTSets{UseDisk: false, CacheSize: 64 * MB, ReaderReadAHead: 95})
	t.Cleanup(func() { settings.StoreBTsets(prev) })

	s := NewStorage()
	h := mkHash(0xE5)
	const numPieces = 700
	s.callbackOpen(1, h, numPieces, plen)
	c := s.CacheByHash(h)
	fileLen := int64(numPieces) * plen
	_, aheadP := c.readerWindowPieces()

	// Old playback connection at piece 300, born a minute ago, ACTIVE.
	rOld := &Reader{cache: c, group: "dev", file: FileInfo{Offset: 0, Length: fileLen}}
	rOld.bornMs = time.Now().UnixMilli() - 60_000
	rOld.offset.Store(300 * plen)
	rOld.winFirst.Store(300)
	rOld.lastRead.Store(time.Now().Unix())
	c.registerReader(rOld)
	if got := c.groupPlayheads()["dev"]; got != 300 {
		t.Fatalf("anchor should commit at 300, got %d", got)
	}

	// Far seek: fresh park from the NEW connection (born now) — instant snap.
	target := 300 + aheadP + 40
	c.setWaitFocus("dev", target, 0, numPieces-1, time.Now().UnixMilli())
	if got := c.groupPlayheads()["dev"]; got != target {
		t.Fatalf("far park: anchor %d, want %d", got, target)
	}

	// The dying old connection parks once more near its position (it was blocked
	// mid-buffer): the anchor must NOT flap back.
	c.setWaitFocus("dev", 302, 0, numPieces-1, rOld.bornMs)
	if got := c.groupPlayheads()["dev"]; got != target {
		t.Fatalf("dying park stole the anchor back to %d, want %d", got, target)
	}
	// ...and it must not overwrite the group focus either: the focus is what keeps
	// the seek target blocked-forced across the new connection's chunk gaps.
	if f, ok := c.recentWaitFocus("dev"); !ok || f.piece != target {
		t.Fatalf("dying park overwrote the group focus (got piece %d, ok=%v), want %d kept", f.piece, ok, target)
	}
	// The same shadow test feeds the blocked-force set: the dying reader's sticky
	// wait at its old position is shadowed, a fresh reader's wait at the target is not.
	if !c.snapShadowed("dev", 302, rOld.bornMs) {
		t.Fatal("dying connection's old-position wait should be snap-shadowed")
	}
	if c.snapShadowed("dev", target, time.Now().UnixMilli()) {
		t.Fatal("fresh wait at the seek target must not be snap-shadowed")
	}

	// Its LIVE read at the old position (still active for staleReaderSec) must
	// not pull the anchor down through the playMin path either.
	rOld.offset.Store(302 * plen)
	rOld.lastRead.Store(time.Now().Unix())
	if got := c.groupPlayheads()["dev"]; got != target {
		t.Fatalf("dying live read pulled the anchor to %d, want %d", got, target)
	}

	// A genuinely NEW connection (born after the snap) seeking back commits
	// instantly — the shadow only covers pre-snap readers.
	c.setWaitFocus("dev", 300, 0, numPieces-1, time.Now().UnixMilli())
	if got := c.groupPlayheads()["dev"]; got != 300 {
		t.Fatalf("fresh seek-back: anchor %d, want 300 at once", got)
	}
}
