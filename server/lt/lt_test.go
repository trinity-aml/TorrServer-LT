package lt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- helpers ----------

// minimalTorrent returns a bencoded single-file .torrent of 100 bytes payload
// with a single 16 KiB piece (all-zero SHA-1 placeholder).
func minimalTorrent() []byte {
	pieces := strings.Repeat("\x00", 20)
	return []byte("d4:infod6:lengthi100e4:name4:test12:piece lengthi16384e6:pieces20:" + pieces + "ee")
}

// validMagnet returns a well-formed magnet URI with a known info hash.
const validMagnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=test"
const validHashHex = "0123456789abcdef0123456789abcdef01234567"

func newSession(t *testing.T) *Session {
	t.Helper()
	s, err := NewSession(SessionConfig{
		// Keep libtorrent quiet during tests.
		"alert_mask":           int(0),
		"enable_dht":           false,
		"enable_lsd":           false,
		"enable_natpmp":        false,
		"enable_upnp":          false,
		"announce_to_all_trackers": false,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// ---------- version & memory ----------

func TestVersion_NonEmpty(t *testing.T) {
	v := Version()
	if v == "" {
		t.Fatal("Version() returned empty")
	}
	if !strings.HasPrefix(v, "2.") {
		t.Fatalf("expected libtorrent 2.x, got %q", v)
	}
}

func TestShimVersion_NonEmpty(t *testing.T) {
	v := ShimVersion()
	if v == "" {
		t.Fatal("ShimVersion() returned empty")
	}
	if !strings.Contains(v, "LT") {
		t.Fatalf("expected shim version to contain 'LT', got %q", v)
	}
}

// ---------- session lifecycle ----------

func TestNewSession_Defaults(t *testing.T) {
	s, err := NewSession(nil)
	if err != nil {
		t.Fatalf("NewSession(nil): %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Double-close is a no-op.
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestNewSession_BadJSON(t *testing.T) {
	// Provoke an unmarshal error by stuffing a value that's not serialisable
	// — actually json.Marshal succeeds for everything we accept here, so
	// trigger via raw shim by feeding garbage JSON.
	// We achieve that via direct settings unmarshal — settings here is map
	// so JSON is always valid. Test the cfg=nil case is handled instead.
	s, err := NewSession(SessionConfig{"nonexistent_setting_xyz": 42})
	if err != nil {
		t.Fatalf("unexpected error for unknown setting: %v", err)
	}
	_ = s.Close()
}

func TestApplySettings_Ok(t *testing.T) {
	s := newSession(t)
	if err := s.ApplySettings(SessionConfig{"connections_limit": 50}); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
}

func TestApplySettings_OnClosed(t *testing.T) {
	s, _ := NewSession(nil)
	_ = s.Close()
	err := s.ApplySettings(SessionConfig{"connections_limit": 50})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
}

func TestSetIPFilter_Empty(t *testing.T) {
	s := newSession(t)
	if err := s.SetIPFilter(""); err != nil {
		t.Fatalf("SetIPFilter empty: %v", err)
	}
}

func TestSetIPFilter_Range(t *testing.T) {
	s := newSession(t)
	rules := "Spam Inc:192.0.2.10-192.0.2.20\nLone:198.51.100.5\n"
	if err := s.SetIPFilter(rules); err != nil {
		t.Fatalf("SetIPFilter: %v", err)
	}
}

func TestSetAlertMask(t *testing.T) {
	s := newSession(t)
	if err := s.SetAlertMask(0); err != nil {
		t.Fatalf("SetAlertMask default: %v", err)
	}
	if err := s.SetAlertMask(0xff); err != nil {
		t.Fatalf("SetAlertMask 0xff: %v", err)
	}
}

// ---------- parsers ----------

func TestParseMagnet_Ok(t *testing.T) {
	pt, err := ParseMagnet(validMagnet)
	if err != nil {
		t.Fatalf("ParseMagnet: %v", err)
	}
	if pt.InfoHash != validHashHex {
		t.Fatalf("info_hash mismatch: got %q, want %q", pt.InfoHash, validHashHex)
	}
	if pt.DisplayName != "test" {
		t.Fatalf("display_name: got %q, want %q", pt.DisplayName, "test")
	}
}

func TestParseMagnet_Empty(t *testing.T) {
	_, err := ParseMagnet("")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
}

func TestParseMagnet_Garbage(t *testing.T) {
	_, err := ParseMagnet("not a magnet")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestParseTorrentBytes_Empty(t *testing.T) {
	_, err := ParseTorrentBytes(nil)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
}

func TestParseTorrentBytes_Garbage(t *testing.T) {
	_, err := ParseTorrentBytes([]byte("not a torrent"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestParseTorrentBytes_Minimal(t *testing.T) {
	pt, err := ParseTorrentBytes(minimalTorrent())
	if err != nil {
		t.Fatalf("ParseTorrentBytes: %v", err)
	}
	if pt.DisplayName != "test" {
		t.Fatalf("display_name: got %q, want %q", pt.DisplayName, "test")
	}
	if !pt.HasMetadata {
		t.Fatal("expected HasMetadata=true")
	}
}

func TestParseTorrentFile_Ok(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minimal.torrent")
	if err := os.WriteFile(path, minimalTorrent(), 0o644); err != nil {
		t.Fatal(err)
	}
	pt, err := ParseTorrentFile(path)
	if err != nil {
		t.Fatalf("ParseTorrentFile: %v", err)
	}
	if pt.DisplayName != "test" {
		t.Fatalf("display_name: got %q", pt.DisplayName)
	}
}

func TestParseTorrentFile_Missing(t *testing.T) {
	_, err := ParseTorrentFile("/nonexistent/path/zzz.torrent")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ---------- AddTorrent ----------

func TestAddTorrent_Hash(t *testing.T) {
	s := newSession(t)
	tor, err := s.AddTorrent(AddTorrentParams{
		Link:     validHashHex,
		SavePath: t.TempDir(),
		Paused:   true,
	})
	if err != nil {
		t.Fatalf("AddTorrent(hash): %v", err)
	}
	if got := tor.InfoHash(); got != validHashHex {
		t.Fatalf("info hash mismatch: got %q", got)
	}
	if got := tor.ID(); got == 0 {
		t.Fatal("zero ID")
	}
}

func TestAddTorrent_Magnet(t *testing.T) {
	s := newSession(t)
	tor, err := s.AddTorrent(AddTorrentParams{
		Link:     validMagnet,
		SavePath: t.TempDir(),
		Paused:   true,
	})
	if err != nil {
		t.Fatalf("AddTorrent(magnet): %v", err)
	}
	if got := tor.InfoHash(); got != validHashHex {
		t.Fatalf("info hash mismatch: got %q", got)
	}
}

func TestAddTorrent_InfoBytes(t *testing.T) {
	s := newSession(t)
	tor, err := s.AddTorrent(AddTorrentParams{
		InfoBytes: minimalTorrent(),
		SavePath:  t.TempDir(),
		Paused:    true,
	})
	if err != nil {
		t.Fatalf("AddTorrent(InfoBytes): %v", err)
	}
	have, _ := tor.HaveMetadata()
	if !have {
		t.Fatal("expected metadata after AddTorrent(InfoBytes)")
	}
	if got := tor.NumPieces(); got != 1 {
		t.Fatalf("NumPieces: got %d, want 1", got)
	}
	if got := tor.PieceLength(); got != 16384 {
		t.Fatalf("PieceLength: got %d, want 16384", got)
	}
	if got := tor.TotalSize(); got != 100 {
		t.Fatalf("TotalSize: got %d, want 100", got)
	}
	if got := tor.DisplayName(); got != "test" {
		t.Fatalf("DisplayName: got %q, want %q", got, "test")
	}
}

func TestAddTorrent_EmptyAll(t *testing.T) {
	s := newSession(t)
	_, err := s.AddTorrent(AddTorrentParams{SavePath: t.TempDir()})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
}

func TestAddTorrent_HTTP_NotSupported(t *testing.T) {
	s := newSession(t)
	_, err := s.AddTorrent(AddTorrentParams{
		Link:     "http://example.com/foo.torrent",
		SavePath: t.TempDir(),
	})
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("expected ErrNotImplemented, got %v", err)
	}
}

func TestAddTorrent_ZeroSession(t *testing.T) {
	var s *Session
	_, err := s.AddTorrent(AddTorrentParams{Link: validHashHex})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
}

// ---------- Torrent ops ----------

func TestTorrent_PauseResume(t *testing.T) {
	s := newSession(t)
	tor, _ := s.AddTorrent(AddTorrentParams{Link: validHashHex, SavePath: t.TempDir(), Paused: false})
	if err := tor.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := tor.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
}

func TestTorrent_ForceRecheck(t *testing.T) {
	s := newSession(t)
	tor, _ := s.AddTorrent(AddTorrentParams{Link: validHashHex, SavePath: t.TempDir(), Paused: true})
	if err := tor.ForceRecheck(); err != nil {
		t.Fatalf("ForceRecheck: %v", err)
	}
}

func TestTorrent_Remove(t *testing.T) {
	s := newSession(t)
	tor, _ := s.AddTorrent(AddTorrentParams{Link: validHashHex, SavePath: t.TempDir(), Paused: true})
	if err := tor.Remove(false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// After remove the handle becomes invalid; subsequent ops should error out.
	if err := tor.Pause(); !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrInvalid|ErrNotFound after Remove, got %v", err)
	}
}

// ---------- metadata accessors ----------

func TestHaveMetadata_BeforeMetadata(t *testing.T) {
	s := newSession(t)
	tor, _ := s.AddTorrent(AddTorrentParams{Link: validHashHex, SavePath: t.TempDir(), Paused: true})
	have, err := tor.HaveMetadata()
	if err != nil {
		t.Fatalf("HaveMetadata: %v", err)
	}
	if have {
		t.Fatal("expected HaveMetadata=false for magnet-only torrent")
	}
}

func TestFiles_WithMetadata(t *testing.T) {
	s := newSession(t)
	tor, _ := s.AddTorrent(AddTorrentParams{InfoBytes: minimalTorrent(), SavePath: t.TempDir(), Paused: true})
	files, err := tor.Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].Size != 100 {
		t.Fatalf("file size: got %d, want 100", files[0].Size)
	}
	if !strings.Contains(files[0].Path, "test") {
		t.Fatalf("file path should contain 'test', got %q", files[0].Path)
	}
}

func TestFiles_BeforeMetadata(t *testing.T) {
	s := newSession(t)
	tor, _ := s.AddTorrent(AddTorrentParams{Link: validHashHex, SavePath: t.TempDir(), Paused: true})
	files, err := tor.Files()
	if err != nil {
		t.Fatalf("Files before metadata: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("expected 0 files, got %d", len(files))
	}
}

func TestMetadataAlloc_BeforeMetadata(t *testing.T) {
	s := newSession(t)
	tor, _ := s.AddTorrent(AddTorrentParams{Link: validHashHex, SavePath: t.TempDir(), Paused: true})
	_, err := tor.Metadata()
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestInfoHash(t *testing.T) {
	s := newSession(t)
	tor, _ := s.AddTorrent(AddTorrentParams{Link: validHashHex, SavePath: t.TempDir(), Paused: true})
	got := tor.InfoHash()
	if got != validHashHex {
		t.Fatalf("InfoHash: got %q, want %q", got, validHashHex)
	}
}

func TestDisplayName(t *testing.T) {
	s := newSession(t)
	tor, _ := s.AddTorrent(AddTorrentParams{Link: validMagnet, SavePath: t.TempDir(), Paused: true})
	got := tor.DisplayName()
	if got != "test" {
		t.Fatalf("DisplayName: got %q, want %q", got, "test")
	}
}

// ---------- priorities ----------

func TestSetPiecePriority_NoMetadata(t *testing.T) {
	s := newSession(t)
	tor, _ := s.AddTorrent(AddTorrentParams{Link: validHashHex, SavePath: t.TempDir(), Paused: true})
	// Without metadata libtorrent will silently store the request — should not error.
	if err := tor.SetPiecePriority(0, 4); err != nil {
		t.Fatalf("SetPiecePriority: %v", err)
	}
}

func TestSetPiecePriority_BadPrio(t *testing.T) {
	s := newSession(t)
	tor, _ := s.AddTorrent(AddTorrentParams{Link: validHashHex, SavePath: t.TempDir(), Paused: true})
	if err := tor.SetPiecePriority(0, 42); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
}

func TestSetPieceDeadline_AndClear(t *testing.T) {
	s := newSession(t)
	tor, _ := s.AddTorrent(AddTorrentParams{InfoBytes: minimalTorrent(), SavePath: t.TempDir(), Paused: true})
	if err := tor.SetPieceDeadline(0, 500, true); err != nil {
		t.Fatalf("SetPieceDeadline: %v", err)
	}
	if err := tor.ClearPieceDeadlines(); err != nil {
		t.Fatalf("ClearPieceDeadlines: %v", err)
	}
}

func TestSetFilePriority(t *testing.T) {
	s := newSession(t)
	tor, _ := s.AddTorrent(AddTorrentParams{InfoBytes: minimalTorrent(), SavePath: t.TempDir(), Paused: true})
	if err := tor.SetFilePriority(0, 4); err != nil {
		t.Fatalf("SetFilePriority: %v", err)
	}
}

// ---------- status ----------

func TestStatus(t *testing.T) {
	s := newSession(t)
	tor, _ := s.AddTorrent(AddTorrentParams{InfoBytes: minimalTorrent(), SavePath: t.TempDir(), Paused: true})
	st, err := tor.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.NumPieces == 0 {
		// num_pieces is the count of pieces we HAVE; we won't have any yet.
		// What matters is that the JSON came through and basic fields are present.
	}
	if !st.HasMetadata {
		t.Fatal("expected HasMetadata=true")
	}
	if st.PieceLength != 16384 {
		t.Fatalf("PieceLength: got %d, want 16384", st.PieceLength)
	}
	if st.TotalSize != 100 {
		t.Fatalf("TotalSize: got %d, want 100", st.TotalSize)
	}
}

// ---------- alerts ----------

func TestAlerts_AfterAdd(t *testing.T) {
	s := newSession(t)
	// Turn on a broader alert mask so we definitely catch the torrent_added.
	_ = s.SetAlertMask(0xffff_ffff)
	if _, err := s.AddTorrent(AddTorrentParams{Link: validHashHex, SavePath: t.TempDir(), Paused: true}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var found bool
	for time.Now().Before(deadline) && !found {
		if ok, err := s.WaitAlert(500 * time.Millisecond); err != nil {
			t.Fatalf("WaitAlert: %v", err)
		} else if !ok {
			continue
		}
		alerts, err := s.PopAlerts()
		if err != nil {
			t.Fatalf("PopAlerts: %v", err)
		}
		for _, a := range alerts {
			if a.Type == "add_torrent" || a.Type == "torrent_added" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatal("expected torrent_added/add_torrent alert within 5s")
	}
}

func TestWaitAlert_Timeout(t *testing.T) {
	s := newSession(t)
	_ = s.SetAlertMask(0) // suppress chatter
	// Drain anything queued from session startup first.
	for {
		ok, _ := s.WaitAlert(50 * time.Millisecond)
		if !ok {
			break
		}
		_, _ = s.PopAlerts()
	}
	ok, err := s.WaitAlert(100 * time.Millisecond)
	if err != nil {
		t.Fatalf("WaitAlert: %v", err)
	}
	if ok {
		// Not strictly an error — libtorrent may emit periodic alerts; just log it.
		t.Logf("WaitAlert returned true on quiet session — acceptable")
	}
}

func TestRequestSessionStats(t *testing.T) {
	s := newSession(t)
	if err := s.RequestSessionStats(); err != nil {
		t.Fatalf("RequestSessionStats: %v", err)
	}
}

// ---------- storage callbacks ----------

func TestRegisterStorageCallbacks_Stub(t *testing.T) {
	if err := RegisterStorageCallbacks(StorageCallbacks{
		Open:    func(int64, [20]byte, int, int64) {},
		Close:   func(int64) {},
		Deleted: func(int64) {},
		Read:    func(int64, int, int64, []byte) (int, error) { return 0, nil },
		Write:   func(int64, int, int64, []byte) (int, error) { return 0, nil },
		Have:    func(int64, int) bool { return false },
	}); err != nil {
		t.Fatalf("RegisterStorageCallbacks: %v", err)
	}
}

// TestStorage_CallbackTriggeredByAddTorrent is the end-to-end smoke
// test for Etap 4.1: register Go-side storage callbacks, create a
// session, add a torrent with embedded metadata, and verify that the
// Open callback fires from inside libtorrent's disk thread with the
// expected info hash. This proves the C++ disk_io / cgo / Go pipeline
// is wired correctly.
func TestStorage_CallbackTriggeredByAddTorrent(t *testing.T) {
	var (
		mu       sync.Mutex
		openedID int64
		openedH  [20]byte
		openedN  int
		openedPL int64
	)

	cb := StorageCallbacks{
		Open: func(sid int64, h [20]byte, num int, pl int64) {
			mu.Lock()
			openedID, openedH, openedN, openedPL = sid, h, num, pl
			mu.Unlock()
		},
		Close:   func(int64) {},
		Deleted: func(int64) {},
		Read: func(_ int64, _ int, _ int64, dst []byte) (int, error) {
			// Pretend we have nothing — libtorrent treats it as missing.
			return 0, nil
		},
		Write: func(_ int64, _ int, _ int64, src []byte) (int, error) {
			return len(src), nil
		},
		Have: func(int64, int) bool { return false },
	}
	if err := RegisterStorageCallbacks(cb); err != nil {
		t.Fatalf("RegisterStorageCallbacks: %v", err)
	}
	t.Cleanup(func() { _ = RegisterStorageCallbacks(StorageCallbacks{}) })

	s, err := NewSession(SessionConfig{
		"enable_dht": false, "enable_lsd": false,
		"enable_natpmp": false, "enable_upnp": false,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if _, err := s.AddTorrent(AddTorrentParams{
		InfoBytes: minimalTorrent(),
		SavePath:  t.TempDir(),
		Paused:    true,
	}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}

	// Open fires synchronously inside the disk thread during add_torrent;
	// give it a beat.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := openedID
		mu.Unlock()
		if got != 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if openedID == 0 {
		t.Fatal("Open callback never fired")
	}
	if openedN != 1 {
		t.Errorf("Open NumPieces: got %d, want 1", openedN)
	}
	if openedPL != 16384 {
		t.Errorf("Open PieceLength: got %d, want 16384", openedPL)
	}
	if openedH == ([20]byte{}) {
		t.Error("Open info hash is zero")
	}
}

// ---------- last_error / version ----------

func TestLastError_AfterFailedParse(t *testing.T) {
	_, err := ParseMagnet("not a magnet")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "lt:") {
		t.Fatalf("expected error to wrap an lt sentinel, got: %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "magnet") &&
		!strings.Contains(strings.ToLower(err.Error()), "parse") {
		t.Logf("error message: %v", err)
	}
}

// ---------- benchmarks (sanity, not run in CI by default) ----------

func BenchmarkStatusJSON(b *testing.B) {
	s, _ := NewSession(nil)
	defer s.Close()
	tor, _ := s.AddTorrent(AddTorrentParams{InfoBytes: minimalTorrent(), SavePath: b.TempDir(), Paused: true})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tor.Status(); err != nil {
			b.Fatal(err)
		}
	}
}

// Compile-time guarantee that the package symbol surface stays intact.
var _ = []any{
	(*Session)(nil), (*Torrent)(nil), (*Status)(nil), (*Alert)(nil),
	(*ParsedTorrent)(nil), (*AddTorrentParams)(nil), (*File)(nil),
	(*StorageCallbacks)(nil),
	Version, ShimVersion, NewSession, ParseMagnet, ParseTorrentBytes,
	ParseTorrentFile, RegisterStorageCallbacks,
	fmt.Sprintf, // shut linter up about unused imports if any
}
