package torrstor

import (
	"bytes"
	"io"
	"testing"
)

// The internal ffprobe probe reader must not be mistaken for a streaming client
// (it shares the cache with a detached preload). It is excluded from
// StreamingReaders — which the preload hand-off gate uses — while still counted
// by ActiveReaders (so the torrent isn't reaped while a probe is in flight).
func TestProbeReader_ExcludedFromStreamingReaders(t *testing.T) {
	c := mkCache(t, pieceLen)

	probe := NewReader(c, nil, FileInfo{Offset: 0, Length: pieceLen}, ProbeReaderGroup)
	if !probe.internal {
		t.Fatal("ProbeReaderGroup reader should be flagged internal")
	}
	if got := c.ActiveReaders(); got != 1 {
		t.Fatalf("ActiveReaders=%d, want 1 (probe is an active reader)", got)
	}
	if got := c.StreamingReaders(); got != 0 {
		t.Fatalf("StreamingReaders=%d, want 0 (probe must not count)", got)
	}

	play := NewReader(c, nil, FileInfo{Offset: 0, Length: pieceLen}, "ip:1.2.3.4")
	if play.internal {
		t.Fatal("a normal device-group reader must not be flagged internal")
	}
	if got := c.StreamingReaders(); got != 1 {
		t.Fatalf("StreamingReaders=%d, want 1 (only the playback reader counts)", got)
	}

	_ = play.Close()
	_ = probe.Close()
	if got := c.ActiveReaders(); got != 0 {
		t.Fatalf("ActiveReaders=%d after Close, want 0", got)
	}
}

// Reading through the internal probe reader must never record a priority window:
// winFirst staying -1 is exactly what makes Close skip the teardown that would
// otherwise zero SetPiecePriority on the head pieces the preload is still
// filling (the "preload stops after bitrate" regression).
func TestProbeReader_NeverRecordsWindow(t *testing.T) {
	c := mkCache(t, 3*pieceLen)
	c.writePiece(0, 0, bytes.Repeat([]byte{'A'}, int(pieceLen)))
	c.SignalPieceComplete(0)

	probe := NewReader(c, nil, FileInfo{Offset: 0, Length: 3 * pieceLen}, ProbeReaderGroup)
	defer probe.Close()

	if got := probe.winFirst.Load(); got != -1 {
		t.Fatalf("winFirst=%d at construction, want -1", got)
	}
	buf := make([]byte, pieceLen)
	if _, err := io.ReadFull(probe, buf); err != nil {
		t.Fatalf("probe read: %v", err)
	}
	// scheduleWindow ran from Read but must have no-oped for the internal reader.
	if got := probe.winFirst.Load(); got != -1 {
		t.Fatalf("winFirst=%d after read, want -1 (internal reader must not schedule a window)", got)
	}
}
