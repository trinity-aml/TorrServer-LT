package torrstor

import (
	"bytes"
	"testing"

	"server/lt"
)

const pieceLen = int64(16 * 1024)

func mkHash(seed byte) [20]byte {
	var h [20]byte
	for i := range h {
		h[i] = seed
	}
	return h
}

func TestStorage_OpenCloseRoundTrip(t *testing.T) {
	s := NewStorage()
	h := mkHash(0xAA)
	s.callbackOpen(1, h, 10, pieceLen)

	c := s.CacheByHash(h)
	if c == nil {
		t.Fatal("CacheByHash returned nil right after Open")
	}
	if c.NumPieces != 10 || c.PieceLength != pieceLen {
		t.Fatalf("cache fields: NumPieces=%d PieceLength=%d", c.NumPieces, c.PieceLength)
	}

	s.callbackClose(1)
	if s.CacheByHash(h) != nil {
		t.Fatal("CacheByHash should be nil after Close")
	}
}

func TestStorage_ReadWriteRoundTrip(t *testing.T) {
	s := NewStorage()
	h := mkHash(0xBB)
	s.callbackOpen(7, h, 4, pieceLen)

	payload := bytes.Repeat([]byte{0x42}, int(pieceLen))
	n, err := s.callbackWrite(7, 0, 0, payload)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("write n: got %d, want %d", n, len(payload))
	}

	dst := make([]byte, len(payload))
	got, err := s.callbackRead(7, 0, 0, dst)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != len(payload) {
		t.Fatalf("read n: got %d, want %d", got, len(payload))
	}
	if !bytes.Equal(dst, payload) {
		t.Fatal("read payload mismatch")
	}
}

func TestStorage_HaveAfterFullPiece(t *testing.T) {
	s := NewStorage()
	h := mkHash(0xCC)
	s.callbackOpen(11, h, 4, pieceLen)

	if s.callbackHave(11, 0) {
		t.Fatal("Have should be false before any write")
	}

	payload := bytes.Repeat([]byte{0x77}, int(pieceLen))
	if _, err := s.callbackWrite(11, 0, 0, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	// In production libtorrent verifies the piece and posts piece_finished;
	// the cache marks Have on that signal, not on the raw write.
	s.CacheByHash(h).SignalPieceComplete(0)
	if !s.callbackHave(11, 0) {
		t.Fatal("Have should be true after the piece is signalled complete")
	}
}

func TestStorage_MultiStorageDisjoint(t *testing.T) {
	s := NewStorage()
	hA, hB := mkHash(0x11), mkHash(0x22)
	s.callbackOpen(1, hA, 4, pieceLen)
	s.callbackOpen(2, hB, 4, pieceLen)

	dataA := bytes.Repeat([]byte{'A'}, 1024)
	dataB := bytes.Repeat([]byte{'B'}, 1024)
	_, _ = s.callbackWrite(1, 0, 0, dataA)
	_, _ = s.callbackWrite(2, 0, 0, dataB)

	bufA := make([]byte, 1024)
	bufB := make([]byte, 1024)
	if _, err := s.callbackRead(1, 0, 0, bufA); err != nil {
		t.Fatalf("readA: %v", err)
	}
	if _, err := s.callbackRead(2, 0, 0, bufB); err != nil {
		t.Fatalf("readB: %v", err)
	}
	if !bytes.Equal(bufA, dataA) {
		t.Fatal("storage 1 leaked into storage 2")
	}
	if !bytes.Equal(bufB, dataB) {
		t.Fatal("storage 2 leaked into storage 1")
	}
}

func TestStorage_ReadMissingPieceFails(t *testing.T) {
	s := NewStorage()
	s.callbackOpen(1, mkHash(1), 4, pieceLen)
	dst := make([]byte, 16)
	if _, err := s.callbackRead(1, 2, 0, dst); err == nil {
		t.Fatal("expected error reading a never-written piece")
	}
}

func TestStorage_Wipe(t *testing.T) {
	s := NewStorage()
	h := mkHash(0xDD)
	s.callbackOpen(3, h, 4, pieceLen)
	payload := bytes.Repeat([]byte{0x55}, int(pieceLen))
	_, _ = s.callbackWrite(3, 0, 0, payload)
	s.CacheByHash(h).SignalPieceComplete(0)

	if !s.callbackHave(3, 0) {
		t.Fatal("precondition: piece should be complete after being signalled")
	}

	s.callbackDeleted(3)

	if s.callbackHave(3, 0) {
		t.Fatal("Have should be false after Deleted")
	}
	// Cache itself is still registered (libtorrent didn't call close).
	if s.CacheByHash(h) == nil {
		t.Fatal("CacheByHash should still return the cache after Deleted")
	}
}

// Smoke test: registering the Storage with lt actually plumbs the
// callbacks. We don't go as far as adding a real torrent — that's the
// integration job for Etap 5.
func TestStorage_InstallUninstall(t *testing.T) {
	s := NewStorage()
	if err := s.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Cleanup(func() { _ = s.Uninstall() })

	// Confirm that we can still construct a session without it blowing up.
	sess, err := lt.NewSession(lt.SessionConfig{"enable_dht": false, "enable_lsd": false})
	if err != nil {
		t.Fatalf("NewSession (with custom storage installed): %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
