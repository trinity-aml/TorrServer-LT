package torrstor

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

// helper: build a Cache for a single-file torrent of size `fileSize`,
// piece length `pieceLen`. file.offset = 0.
func mkCache(t *testing.T, fileSize int64) *Cache {
	t.Helper()
	s := NewStorage()
	h := mkHash(0x77)
	numPieces := int((fileSize + pieceLen - 1) / pieceLen)
	s.callbackOpen(1, h, numPieces, pieceLen)
	c := s.CacheByHash(h)
	if c == nil {
		t.Fatal("cache not registered after Open")
	}
	return c
}

func TestReader_ReadAfterPieceWritten(t *testing.T) {
	c := mkCache(t, 3*pieceLen)
	// write piece 0 and 1
	p0 := bytes.Repeat([]byte{'A'}, int(pieceLen))
	p1 := bytes.Repeat([]byte{'B'}, int(pieceLen))
	if _, err := c.writePiece(0, 0, p0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.writePiece(1, 0, p1); err != nil {
		t.Fatal(err)
	}
	// Completion is driven by libtorrent's piece_finished_alert in production;
	// simulate it here now that Piece.WriteAt no longer auto-marks complete.
	c.SignalPieceComplete(0)
	c.SignalPieceComplete(1)

	r := NewReader(c, nil, FileInfo{Offset: 0, Length: 3 * pieceLen})
	defer r.Close()

	buf := make([]byte, 2*pieceLen)
	n, err := io.ReadFull(r, buf)
	if err != nil {
		t.Fatalf("read: n=%d err=%v", n, err)
	}
	if n != len(buf) {
		t.Fatalf("short read: got %d, want %d", n, len(buf))
	}
	if !bytes.Equal(buf[:pieceLen], p0) {
		t.Fatal("piece 0 content mismatch")
	}
	if !bytes.Equal(buf[pieceLen:], p1) {
		t.Fatal("piece 1 content mismatch")
	}
}

func TestReader_Seek(t *testing.T) {
	c := mkCache(t, 3*pieceLen)
	p0 := bytes.Repeat([]byte{'X'}, int(pieceLen))
	p2 := bytes.Repeat([]byte{'Z'}, int(pieceLen))
	_, _ = c.writePiece(0, 0, p0)
	_, _ = c.writePiece(2, 0, p2)
	c.SignalPieceComplete(0)
	c.SignalPieceComplete(2)

	r := NewReader(c, nil, FileInfo{Offset: 0, Length: 3 * pieceLen})
	defer r.Close()

	// Seek to start of piece 2 and read.
	if pos, err := r.Seek(2*pieceLen, io.SeekStart); err != nil || pos != 2*pieceLen {
		t.Fatalf("seek: pos=%d err=%v", pos, err)
	}
	buf := make([]byte, pieceLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read after seek: %v", err)
	}
	if !bytes.Equal(buf, p2) {
		t.Fatal("piece 2 content mismatch after seek")
	}
}

func TestReader_BlocksUntilPieceComplete(t *testing.T) {
	c := mkCache(t, pieceLen)
	r := NewReader(c, nil, FileInfo{Offset: 0, Length: pieceLen})
	defer r.Close()

	// Background producer: after 50ms, write the piece and signal it.
	payload := bytes.Repeat([]byte{0x42}, int(pieceLen))
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = c.writePiece(0, 0, payload)
		c.SignalPieceComplete(0)
	}()

	buf := make([]byte, pieceLen)
	start := time.Now()
	n, err := io.ReadFull(r, buf)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("short read")
	}
	if !bytes.Equal(buf, payload) {
		t.Fatal("content mismatch")
	}
	if took < 40*time.Millisecond {
		t.Logf("warning: read returned in %v (expected ~50ms wait)", took)
	}
}

func TestReader_TimeoutOnMissingPiece(t *testing.T) {
	c := mkCache(t, pieceLen)
	r := NewReader(c, nil, FileInfo{Offset: 0, Length: pieceLen})
	defer r.Close()

	prev := ReaderTimeout
	ReaderTimeout = 100 * time.Millisecond
	t.Cleanup(func() { ReaderTimeout = prev })

	buf := make([]byte, pieceLen)
	_, err := r.Read(buf)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestReader_CloseIsIdempotent(t *testing.T) {
	c := mkCache(t, pieceLen)
	r := NewReader(c, nil, FileInfo{Offset: 0, Length: pieceLen})
	if err := r.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestReader_ActiveReaderCountTracksLifecycle(t *testing.T) {
	c := mkCache(t, pieceLen)
	if got := c.ActiveReaders(); got != 0 {
		t.Fatalf("initial readers=%d, want 0", got)
	}
	r := NewReader(c, nil, FileInfo{Offset: 0, Length: pieceLen})
	if got := c.ActiveReaders(); got != 1 {
		t.Fatalf("after NewReader readers=%d, want 1", got)
	}
	_ = r.Close()
	if got := c.ActiveReaders(); got != 0 {
		t.Fatalf("after Close readers=%d, want 0", got)
	}
}

func TestReader_WaitForPiece_FastPath(t *testing.T) {
	c := mkCache(t, pieceLen)
	_, _ = c.writePiece(0, 0, bytes.Repeat([]byte{0x99}, int(pieceLen)))
	c.SignalPieceComplete(0)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if !c.WaitForPiece(ctx, 0) {
		t.Fatal("WaitForPiece should hit the fast path for a complete piece")
	}
}

func TestReader_WaitForPiece_ContextCancel(t *testing.T) {
	c := mkCache(t, pieceLen)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if c.WaitForPiece(ctx, 0) {
		t.Fatal("WaitForPiece should report false when context fires before signal")
	}
}

func TestReader_PartialReadBoundary(t *testing.T) {
	// File spans 1.5 pieces: 1 full piece + half a piece. Reader must
	// stop at file.Length and surface io.EOF for the next call.
	half := pieceLen / 2
	totalLen := pieceLen + half

	c := mkCache(t, totalLen)
	full := bytes.Repeat([]byte{'F'}, int(pieceLen))
	tail := bytes.Repeat([]byte{'T'}, int(half))
	_, _ = c.writePiece(0, 0, full)
	_, _ = c.writePiece(1, 0, tail)
	// Completion is signalled explicitly (Piece.WriteAt no longer auto-marks
	// complete — production relies on libtorrent's piece_finished_alert).
	c.SignalPieceComplete(0)
	c.SignalPieceComplete(1)

	r := NewReader(c, nil, FileInfo{Offset: 0, Length: totalLen})
	defer r.Close()
	buf, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if int64(len(buf)) != totalLen {
		t.Fatalf("len=%d want %d", len(buf), totalLen)
	}
	if !bytes.Equal(buf[:pieceLen], full) || !bytes.Equal(buf[pieceLen:], tail) {
		t.Fatal("file content mismatch")
	}
}
