package torrstor

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"server/lt"
	"server/settings"
)

// makeSyntheticTorrent crafts a bencoded single-file v1 .torrent for the
// given content, with piece_length == len(content) (one piece total).
// Returned `info` is the raw info-dict bytes; sha1(info) is the
// torrent's info hash.
func makeSyntheticTorrent(content []byte, name string) (torrentBytes, infoSection []byte, infoHash [20]byte) {
	pieceSha := sha1.Sum(content)
	infoSection = []byte(fmt.Sprintf(
		"d6:lengthi%de4:name%d:%s12:piece lengthi%de6:pieces20:%se",
		len(content), len(name), name, len(content), string(pieceSha[:]),
	))
	torrentBytes = []byte("d4:info")
	torrentBytes = append(torrentBytes, infoSection...)
	torrentBytes = append(torrentBytes, 'e')
	infoHash = sha1.Sum(infoSection)
	return
}

// TestE2E_LocalSessionReaderReadsHavePiece is the in-process smoke test
// for the full Etap 4–5 chain:
//
//   - synthetic 1-piece torrent + matching content pre-written under
//     TorrentsSavePath/<hash>/0
//   - install our storage callbacks
//   - lt.NewSession picks up the custom disk_io
//   - AddTorrent(InfoBytes + HavePieces=[1]) tells libtorrent it
//     already has the piece, with `no_verify_files` skipping the hash
//     recheck (the trust-file-sizes resume policy)
//   - torrstor.NewReader pulls the bytes out through the same Cache
//     instance libtorrent now owns
//
// No network involved — peer connectivity, DHT, LSD, NAT-PMP are all
// disabled in the session config.
func TestE2E_LocalSessionReaderReadsHavePiece(t *testing.T) {
	// 1. Prepare a 16 KiB random-looking payload (deterministic for
	//    reproducibility) and the matching .torrent.
	content := make([]byte, 16*1024)
	for i := range content {
		content[i] = byte((i * 7) & 0xff)
	}
	torrentBytes, _, infoHash := makeSyntheticTorrent(content, "smoke.bin")

	// 2. Point UseDisk at a temp dir and pre-stage the piece file the
	//    scan/resume path will find.
	prev := settings.BTsets
	dir := t.TempDir()
	settings.BTsets = &settings.BTSets{
		UseDisk:          true,
		TorrentsSavePath: dir,
		CacheSize:        0, // unlimited for this test
	}
	t.Cleanup(func() { settings.BTsets = prev })

	pieceDir := filepath.Join(dir, hashHex(infoHash))
	if err := os.MkdirAll(pieceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pieceDir, "0"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	// 3. Wire the Go storage backend into libtorrent's disk_io.
	s := NewStorage()
	if err := s.Install(); err != nil {
		t.Fatalf("Install storage: %v", err)
	}
	t.Cleanup(func() { _ = s.Uninstall() })

	// 4. Boot a quiet session.
	sess, err := lt.NewSession(lt.SessionConfig{
		"enable_dht":    false,
		"enable_lsd":    false,
		"enable_natpmp": false,
		"enable_upnp":   false,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	// 5. Add the torrent with HavePieces=[bit0 set].
	tor, err := sess.AddTorrent(lt.AddTorrentParams{
		InfoBytes:  torrentBytes,
		HavePieces: []byte{0x01},
		PieceCount: 1,
		SavePath:   dir,
		Paused:     true,
	})
	if err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	t.Cleanup(func() { _ = tor.Remove(false) })

	// 6. Open fires synchronously inside the disk thread; wait for the
	//    Cache to be visible from Go-side.
	deadline := time.Now().Add(2 * time.Second)
	var cache *Cache
	for time.Now().Before(deadline) {
		if cache = s.CacheByHash(infoHash); cache != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cache == nil {
		t.Fatal("Storage.Open never registered the Cache")
	}

	// 7. Read the file via the streaming Reader. Reader.Read will
	//    lazily reconstruct the Piece from the pre-staged disk file
	//    (the resume read path) on the first hit.
	r := NewReader(cache, tor, FileInfo{Index: 0, Path: "smoke.bin", Offset: 0, Length: int64(len(content))})
	if r == nil {
		t.Fatal("NewReader returned nil")
	}
	defer r.Close()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("payload mismatch: got %d bytes, want %d", len(got), len(content))
	}
}
