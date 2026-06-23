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
