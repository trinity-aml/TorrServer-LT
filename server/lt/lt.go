// Package lt is the Go-side facade over the libtorrent (arvidn) C-shim.
//
// All cross-FFI work funnels through this package; the rest of the server
// must NOT import "C" or touch lt_shim.h directly. The package exposes
// Go-idiomatic types (Session, Torrent, Status, Alert) and a small set of
// errors mirroring the LT_ERR_* codes from lt_shim.h.
//
// Threading:
//   - Session and Torrent methods are safe to call from any goroutine.
//   - WaitAlert / PopAlerts must be driven from at most one goroutine per
//     Session; the shim serialises them under an internal mutex, but the
//     intent is a dedicated alert-pump goroutine on the Go side.
//
// Memory:
//   - The shim never holds Go pointers across C calls; all data crosses as
//     byte buffers or JSON-encoded strings, which are copied into Go space
//     before being returned.
package lt

/*
#cgo CFLAGS: -I${SRCDIR}
#cgo CXXFLAGS: -std=c++17 -I${SRCDIR} -I${SRCDIR}/third_party
#cgo pkg-config: libtorrent-rasterbar
#cgo LDFLAGS: -lstdc++

#include <stdlib.h>
#include "lt_shim.h"
#include "lt_disk_io.h"

int tsl_install_go_storage_callbacks(void);
int tsl_uninstall_go_storage_callbacks(void);
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"
)

// ----- error codes -----

var (
	ErrInvalid        = errors.New("lt: invalid argument")
	ErrNotFound       = errors.New("lt: not found")
	ErrTimeout        = errors.New("lt: timeout")
	ErrIO             = errors.New("lt: i/o error")
	ErrParse          = errors.New("lt: parse error")
	ErrNotImplemented = errors.New("lt: not implemented")
	ErrInternal       = errors.New("lt: internal error")
)

func codeToErr(code C.int) error {
	switch code {
	case C.LT_OK:
		return nil
	case C.LT_ERR_INVALID:
		return wrap(ErrInvalid)
	case C.LT_ERR_NOT_FOUND:
		return wrap(ErrNotFound)
	case C.LT_ERR_TIMEOUT:
		return wrap(ErrTimeout)
	case C.LT_ERR_IO:
		return wrap(ErrIO)
	case C.LT_ERR_PARSE:
		return wrap(ErrParse)
	case C.LT_ERR_NOT_IMPL:
		return wrap(ErrNotImplemented)
	default:
		return wrap(ErrInternal)
	}
}

func wrap(base error) error {
	msg := C.GoString(C.lt_last_error())
	if msg == "" {
		return base
	}
	return fmt.Errorf("%w: %s", base, msg)
}

// baseForCode maps a numeric LT_ERR_* to its Go sentinel, defaulting to
// ErrInternal for unknown values.
func baseForCode(code C.int) error {
	switch code {
	case C.LT_OK:
		return nil
	case C.LT_ERR_INVALID:
		return ErrInvalid
	case C.LT_ERR_NOT_FOUND:
		return ErrNotFound
	case C.LT_ERR_TIMEOUT:
		return ErrTimeout
	case C.LT_ERR_IO:
		return ErrIO
	case C.LT_ERR_PARSE:
		return ErrParse
	case C.LT_ERR_NOT_IMPL:
		return ErrNotImplemented
	default:
		return ErrInternal
	}
}

// lastError reads the thread-local code+message left by the most recent
// failed shim call and wraps the corresponding Go sentinel.
func lastError() error {
	base := baseForCode(C.lt_last_error_code())
	if base == nil {
		base = ErrInternal
	}
	msg := C.GoString(C.lt_last_error())
	if msg == "" {
		return base
	}
	return fmt.Errorf("%w: %s", base, msg)
}

// ----- versions -----

// Version returns the libtorrent version string the shim was compiled against,
// e.g. "2.0.10.0".
func Version() string {
	return cStringBuf(func(buf *C.char, cap C.size_t) C.size_t {
		return C.lt_engine_version(buf, cap)
	}, 32)
}

// ShimVersion returns the TorrServer-LT shim's own version tag.
func ShimVersion() string {
	return cStringBuf(func(buf *C.char, cap C.size_t) C.size_t {
		return C.lt_shim_version(buf, cap)
	}, 32)
}

// ----- low-level buffer helpers -----

// cStringBuf invokes a "fill caller-provided buffer" shim function. It first
// tries `initialCap` and retries once with the exact size the shim reported
// if the value did not fit.
func cStringBuf(call func(*C.char, C.size_t) C.size_t, initialCap int) string {
	if initialCap < 16 {
		initialCap = 16
	}
	buf := make([]byte, initialCap)
	n := call((*C.char)(unsafe.Pointer(&buf[0])), C.size_t(initialCap))
	if int(n) > initialCap {
		buf = make([]byte, int(n)+1)
		n = call((*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)))
	}
	if int(n) == 0 {
		return ""
	}
	return string(buf[:int(n)])
}

// cAlloc invokes a "shim allocates a buffer" function and returns the
// captured Go bytes (the C buffer is freed afterwards via lt_free).
func cAlloc(call func(*C.size_t) *C.char) ([]byte, error) {
	var n C.size_t
	p := call(&n)
	if p == nil {
		return nil, lastError()
	}
	defer C.lt_free(unsafe.Pointer(p))
	if n == 0 {
		return nil, nil
	}
	return C.GoBytes(unsafe.Pointer(p), C.int(n)), nil
}

// ----- Session -----

// Session wraps a libtorrent session.
type Session struct {
	id C.lt_session
}

// SessionConfig is a thin alias over the JSON-encodable settings_pack dict.
// Use NewSession(SessionConfig{...}) to start a session with non-default
// settings. Refer to libtorrent's settings_pack documentation for the
// recognised keys.
type SessionConfig map[string]any

// NewSession constructs a libtorrent session with the given settings.
// Pass nil for defaults.
func NewSession(cfg SessionConfig) (*Session, error) {
	var cs *C.char
	if cfg != nil {
		b, err := json.Marshal(cfg)
		if err != nil {
			return nil, fmt.Errorf("%w: marshal settings: %v", ErrParse, err)
		}
		cs = C.CString(string(b))
		defer C.free(unsafe.Pointer(cs))
	}
	id := C.lt_session_new(cs)
	if id == 0 {
		return nil, lastError()
	}
	return &Session{id: id}, nil
}

// Close destroys the session and drops all torrents (without persisting).
// After Close the Session is unusable.
func (s *Session) Close() error {
	if s == nil || s.id == 0 {
		return nil
	}
	rc := C.lt_session_destroy(s.id)
	s.id = 0
	return codeToErr(rc)
}

// ApplySettings updates a running session.
func (s *Session) ApplySettings(cfg SessionConfig) error {
	if s == nil || s.id == 0 {
		return ErrInvalid
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("%w: marshal settings: %v", ErrParse, err)
	}
	cs := C.CString(string(b))
	defer C.free(unsafe.Pointer(cs))
	return codeToErr(C.lt_session_apply_settings(s.id, cs))
}

// SetIPFilter installs an IP filter from a P2P-format text block.
// Pass "" to clear the filter.
func (s *Session) SetIPFilter(p2pText string) error {
	if s == nil || s.id == 0 {
		return ErrInvalid
	}
	cs := C.CString(p2pText)
	defer C.free(unsafe.Pointer(cs))
	return codeToErr(C.lt_session_set_ip_filter(s.id, cs))
}

// SetAlertMask installs the libtorrent alert_mask. Pass 0 for defaults.
func (s *Session) SetAlertMask(mask uint32) error {
	if s == nil || s.id == 0 {
		return ErrInvalid
	}
	return codeToErr(C.lt_session_set_alert_mask(s.id, C.uint32_t(mask)))
}

// ----- AddTorrent -----

// AddTorrentParams describes what to load.
type AddTorrentParams struct {
	// Link may be a magnet URI, a 40-hex info hash, or empty when InfoBytes
	// carries the .torrent payload.
	Link string
	// InfoBytes is the raw .torrent file. Mutually exclusive with magnet/hash
	// links (if both are set, InfoBytes takes precedence).
	InfoBytes []byte
	// Trackers is a list of announce URLs appended on top of whatever the
	// link or metadata already carries.
	Trackers []string
	// SavePath is the directory libtorrent's default storage would use
	// (ignored once the custom storage is wired in Etap 4).
	SavePath string
	// Paused starts the torrent paused.
	Paused bool
	// HavePieces is a packed bitmap (LSB-first) indicating pieces that
	// are already present locally. PieceCount must be set to the number
	// of valid bits (typically the torrent's piece count). When non-empty,
	// libtorrent skips the post-add hash verification and treats the
	// listed pieces as known-complete (matches the "trust file-sizes"
	// resume policy).
	HavePieces []byte
	PieceCount int
}

// AddTorrent adds a torrent to the session and returns a handle.
func (s *Session) AddTorrent(p AddTorrentParams) (*Torrent, error) {
	if s == nil || s.id == 0 {
		return nil, ErrInvalid
	}
	var (
		cLink     *C.char
		cTrackers *C.char
		cSave     *C.char
		cInfo     *C.uint8_t
		cInfoLen  C.size_t
	)
	if p.Link != "" {
		cLink = C.CString(p.Link)
		defer C.free(unsafe.Pointer(cLink))
	}
	if len(p.Trackers) > 0 {
		joined := ""
		for i, t := range p.Trackers {
			if i > 0 {
				joined += ","
			}
			joined += t
		}
		cTrackers = C.CString(joined)
		defer C.free(unsafe.Pointer(cTrackers))
	}
	if p.SavePath != "" {
		cSave = C.CString(p.SavePath)
		defer C.free(unsafe.Pointer(cSave))
	}
	if len(p.InfoBytes) > 0 {
		cInfo = (*C.uint8_t)(unsafe.Pointer(&p.InfoBytes[0]))
		cInfoLen = C.size_t(len(p.InfoBytes))
	}
	paused := C.int(0)
	if p.Paused {
		paused = 1
	}
	var (
		cHaveBuf  *C.uint8_t
		cHaveBits C.int
	)
	if len(p.HavePieces) > 0 && p.PieceCount > 0 {
		cHaveBuf = (*C.uint8_t)(unsafe.Pointer(&p.HavePieces[0]))
		cHaveBits = C.int(p.PieceCount)
	}
	id := C.lt_session_add_torrent(s.id, cLink, cInfo, cInfoLen, cTrackers, cSave, paused, cHaveBuf, cHaveBits)
	if id == 0 {
		return nil, lastError()
	}
	return &Torrent{sess: s, id: id}, nil
}

// ----- Torrent -----

// Torrent is a handle to a single torrent inside a Session.
type Torrent struct {
	sess *Session
	id   C.lt_torrent
}

// ID returns the opaque shim handle.
func (t *Torrent) ID() int64 { return int64(t.id) }

// Remove deletes the torrent from the session. If deleteFiles is true,
// libtorrent also unlinks the on-disk data (currently unused — see Etap 4).
func (t *Torrent) Remove(deleteFiles bool) error {
	if t == nil || t.id == 0 || t.sess == nil {
		return ErrInvalid
	}
	df := C.int(0)
	if deleteFiles {
		df = 1
	}
	rc := C.lt_torrent_remove(t.sess.id, t.id, df)
	if rc == C.LT_OK {
		t.id = 0
	}
	return codeToErr(rc)
}

func (t *Torrent) Pause() error        { return codeToErr(C.lt_torrent_pause(t.id)) }
func (t *Torrent) Resume() error       { return codeToErr(C.lt_torrent_resume(t.id)) }
func (t *Torrent) ForceRecheck() error { return codeToErr(C.lt_torrent_force_recheck(t.id)) }

// ForceReannounce re-announces to all trackers immediately (ignoring the min
// interval). ForceDhtAnnounce does the same for the DHT. Used at playback start
// to find peers fast for a lazily-added torrent.
func (t *Torrent) ForceReannounce() error  { return codeToErr(C.lt_torrent_force_reannounce(t.id)) }
func (t *Torrent) ForceDhtAnnounce() error { return codeToErr(C.lt_torrent_force_dht_announce(t.id)) }

// HaveMetadata reports whether libtorrent has the .torrent's info dict yet.
func (t *Torrent) HaveMetadata() (bool, error) {
	rc := C.lt_torrent_have_metadata(t.id)
	if rc < 0 {
		return false, codeToErr(rc)
	}
	return rc != 0, nil
}

// Metadata returns the raw info-dict bytes once metadata has been received.
func (t *Torrent) Metadata() ([]byte, error) {
	return cAlloc(func(n *C.size_t) *C.char {
		return C.lt_torrent_metadata_alloc(t.id, n)
	})
}

// File describes a single entry inside the torrent.
type File struct {
	Index  int
	Path   string
	Size   int64
	Offset int64
}

// Files returns the full file list once metadata is available.
func (t *Torrent) Files() ([]File, error) {
	n := int(C.lt_torrent_num_files(t.id))
	if n <= 0 {
		return nil, nil
	}
	out := make([]File, 0, n)
	for i := 0; i < n; i++ {
		idx := C.int(i)
		path := cStringBuf(func(buf *C.char, cap C.size_t) C.size_t {
			return C.lt_torrent_file_path(t.id, idx, buf, cap)
		}, 256)
		size := int64(C.lt_torrent_file_size(t.id, idx))
		off := int64(C.lt_torrent_file_offset(t.id, idx))
		if size < 0 || off < 0 {
			return out, lastError()
		}
		out = append(out, File{Index: i, Path: path, Size: size, Offset: off})
	}
	return out, nil
}

// NumPieces returns the total piece count (0 before metadata).
func (t *Torrent) NumPieces() int { return int(C.lt_torrent_num_pieces(t.id)) }

// PieceLength returns the piece length in bytes (0 before metadata).
func (t *Torrent) PieceLength() int64 { return int64(C.lt_torrent_piece_length(t.id)) }

// TotalSize returns the sum of file sizes (0 before metadata).
func (t *Torrent) TotalSize() int64 { return int64(C.lt_torrent_total_size(t.id)) }

// DisplayName returns the torrent name (set on the torrent or magnet's dn=).
func (t *Torrent) DisplayName() string {
	return cStringBuf(func(buf *C.char, cap C.size_t) C.size_t {
		return C.lt_torrent_display_name(t.id, buf, cap)
	}, 128)
}

// InfoHash returns the v1 SHA-1 info hash as a 40-char lowercase hex string.
func (t *Torrent) InfoHash() string {
	return cStringBuf(func(buf *C.char, cap C.size_t) C.size_t {
		return C.lt_torrent_info_hash_hex(t.id, buf, cap)
	}, 41)
}

// SetPiecePriority sets the per-piece download priority (0..7).
func (t *Torrent) SetPiecePriority(piece, prio int) error {
	return codeToErr(C.lt_torrent_set_piece_priority(t.id, C.int(piece), C.int(prio)))
}

// SetAllPiecesPriority sets every piece to prio in one call. Used to switch a
// torrent to lazy/streaming mode (prio 0 = download nothing until a Reader or
// Preload bumps the pieces it actually needs).
func (t *Torrent) SetAllPiecesPriority(prio int) error {
	return codeToErr(C.lt_torrent_set_all_pieces_priority(t.id, C.int(prio)))
}

// WeDontHave tells libtorrent's piece_picker to forget that it has the given
// piece, so the picker will re-request it from peers. The streaming cache calls
// this when it evicts a piece, keeping libtorrent's have-bitfield in sync with
// what is actually buffered — without it a seek back into an evicted region
// could never be re-downloaded. Also clears the piece deadline and lowers its
// priority (the reader's window re-raises it on demand).
func (t *Torrent) WeDontHave(piece int) error {
	return codeToErr(C.lt_torrent_we_dont_have(t.id, C.int(piece)))
}

// SetPieceDeadline sets a soft deadline (in ms) for a piece, optionally
// asking for an alert when the piece is ready.
func (t *Torrent) SetPieceDeadline(piece, deadlineMs int, alertWhenReady bool) error {
	a := C.int(0)
	if alertWhenReady {
		a = 1
	}
	return codeToErr(C.lt_torrent_set_piece_deadline(t.id, C.int(piece), C.int(deadlineMs), a))
}

// ClearPieceDeadlines drops all streaming deadlines on this torrent.
func (t *Torrent) ClearPieceDeadlines() error {
	return codeToErr(C.lt_torrent_clear_piece_deadlines(t.id))
}

// SetFilePriority sets the priority for a file (0..7).
func (t *Torrent) SetFilePriority(file, prio int) error {
	return codeToErr(C.lt_torrent_set_file_priority(t.id, C.int(file), C.int(prio)))
}

// Status is a snapshot of a torrent's runtime state, as exposed by the
// shim's JSON encoding.
type Status struct {
	Name                 string  `json:"name"`
	InfoHash             string  `json:"info_hash"`
	State                string  `json:"state"`
	IsFinished           bool    `json:"is_finished"`
	Progress             float64 `json:"progress"`
	TotalDone            int64   `json:"total_done"`
	TotalWanted          int64   `json:"total_wanted"`
	DownloadRate         int     `json:"download_rate"`
	UploadRate           int     `json:"upload_rate"`
	NumPeers             int     `json:"num_peers"`
	NumSeeds             int     `json:"num_seeds"`
	ListPeers            int     `json:"list_peers"`
	ListSeeds            int     `json:"list_seeds"`
	ConnectCandidates    int     `json:"connect_candidates"`
	TotalPayloadDownload int64   `json:"total_payload_download"`
	TotalPayloadUpload   int64   `json:"total_payload_upload"`
	TotalDownload        int64   `json:"total_download"`
	TotalUpload          int64   `json:"total_upload"`
	NumPieces            int     `json:"num_pieces"`
	PieceLength          int64   `json:"piece_length"`
	TotalSize            int64   `json:"total_size"`
	HasMetadata          bool    `json:"has_metadata"`
}

// Status returns the current torrent_status, decoded from the shim's JSON.
func (t *Torrent) Status() (*Status, error) {
	raw := cStringBuf(func(buf *C.char, cap C.size_t) C.size_t {
		return C.lt_torrent_status_json(t.id, buf, cap)
	}, 1024)
	if raw == "" {
		return nil, lastError()
	}
	var st Status
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil, fmt.Errorf("%w: status decode: %v", ErrInternal, err)
	}
	return &st, nil
}

// ----- alerts -----

// Alert is one entry produced by the shim's alert pump.
type Alert struct {
	Type        string          `json:"type"`
	Category    uint32          `json:"category"`
	Message     string          `json:"message"`
	Torrent     int64           `json:"torrent,omitempty"`
	TorrentHash string          `json:"torrent_hash,omitempty"`
	Piece       int             `json:"piece,omitempty"`
	Block       int             `json:"block,omitempty"`
	URL         string          `json:"url,omitempty"`
	Peers       int             `json:"peers,omitempty"`
	Error       string          `json:"error,omitempty"`
	Raw         json.RawMessage `json:"-"`
}

// WaitAlert blocks until at least one alert is available, or timeout elapses.
// Returns true if at least one alert is ready.
func (s *Session) WaitAlert(timeout time.Duration) (bool, error) {
	ms := C.int(timeout / time.Millisecond)
	if ms < 0 {
		ms = -1
	}
	rc := C.lt_session_wait_alert(s.id, ms)
	if rc < 0 {
		return false, codeToErr(rc)
	}
	return rc > 0, nil
}

// PopAlerts drains the alert queue.
func (s *Session) PopAlerts() ([]Alert, error) {
	raw, err := cAlloc(func(n *C.size_t) *C.char {
		return C.lt_session_pop_alerts_json_alloc(s.id, n)
	})
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var arr []Alert
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("%w: alert decode: %v", ErrInternal, err)
	}
	// Re-attach raw payload per alert for callers that need fields outside
	// the typed struct.
	var rawArr []json.RawMessage
	if jsonErr := json.Unmarshal(raw, &rawArr); jsonErr == nil && len(rawArr) == len(arr) {
		for i := range arr {
			arr[i].Raw = rawArr[i]
		}
	}
	return arr, nil
}

// RequestSessionStats fires off a session_stats request — the actual values
// arrive asynchronously through the alert pump.
func (s *Session) RequestSessionStats() error {
	_, err := cAlloc(func(n *C.size_t) *C.char {
		return C.lt_session_stats_json_alloc(s.id, n)
	})
	return err
}

// ----- parsers -----

// ParsedTorrent is the decoded form of a magnet URI or .torrent file as
// emitted by the shim's parsers.
type ParsedTorrent struct {
	InfoHash     string   `json:"info_hash"`
	DisplayName  string   `json:"display_name"`
	Trackers     []string `json:"trackers"`
	HasMetadata  bool     `json:"has_metadata"`
	MetadataSize int      `json:"metadata_size"`
	NumPieces    int      `json:"num_pieces,omitempty"`
	PieceLength  int64    `json:"piece_length,omitempty"`
	TotalSize    int64    `json:"total_size,omitempty"`
}

// ParseMagnet parses a magnet URI and returns its info hash, display name and
// trackers (no metadata fetching is performed).
func ParseMagnet(uri string) (*ParsedTorrent, error) {
	cs := C.CString(uri)
	defer C.free(unsafe.Pointer(cs))
	raw, err := cAlloc(func(n *C.size_t) *C.char {
		return C.lt_parse_magnet_alloc(cs, n)
	})
	if err != nil {
		return nil, err
	}
	return decodeParsed(raw)
}

// ParseTorrentBytes parses an in-memory .torrent payload.
func ParseTorrentBytes(buf []byte) (*ParsedTorrent, error) {
	if len(buf) == 0 {
		return nil, ErrInvalid
	}
	raw, err := cAlloc(func(n *C.size_t) *C.char {
		return C.lt_parse_torrent_bytes_alloc(
			(*C.uint8_t)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), n)
	})
	if err != nil {
		return nil, err
	}
	return decodeParsed(raw)
}

// ParseTorrentFile parses a .torrent file from disk.
func ParseTorrentFile(path string) (*ParsedTorrent, error) {
	cs := C.CString(path)
	defer C.free(unsafe.Pointer(cs))
	raw, err := cAlloc(func(n *C.size_t) *C.char {
		return C.lt_parse_torrent_file_alloc(cs, n)
	})
	if err != nil {
		return nil, err
	}
	return decodeParsed(raw)
}

func decodeParsed(raw []byte) (*ParsedTorrent, error) {
	if len(raw) == 0 {
		return nil, lastError()
	}
	var pt ParsedTorrent
	if err := json.Unmarshal(raw, &pt); err != nil {
		return nil, fmt.Errorf("%w: parsed decode: %v", ErrInternal, err)
	}
	return &pt, nil
}

// ----- storage callbacks (custom disk_io) -----

// StorageCallbacks holds Go-side handlers that, once registered, libtorrent
// calls from its disk thread pool. Etap 4.1 ships an in-memory backing
// store implementation; Etap 4.2 adds on-disk pieces and resume.
//
// All handlers must be safe to call from any goroutine and may execute on
// libtorrent's disk-io threads. Returning an error from Read/Write surfaces
// as a storage_error on the libtorrent side.
type StorageCallbacks struct {
	// Open is fired when libtorrent creates a new storage for a torrent.
	Open func(storage int64, infoHash [20]byte, numPieces int, pieceLength int64)
	// Close is fired when libtorrent destroys a storage.
	Close func(storage int64)
	// Deleted is fired when libtorrent requests deletion of the on-disk
	// files for a storage (e.g. RemTorrent with delete=true).
	Deleted func(storage int64)
	// Read populates dst with `len(dst)` bytes from the (piece, offset)
	// range; returns the bytes actually copied (must equal len(dst) on
	// success).
	Read func(storage int64, piece int, offset int64, dst []byte) (int, error)
	// Write persists src bytes into the (piece, offset) range; returns
	// the bytes actually persisted.
	Write func(storage int64, piece int, offset int64, src []byte) (int, error)
	// Have reports whether the piece is locally complete (used in Etap 4.2
	// resume scan).
	Have func(storage int64, piece int) bool
}

var (
	storageMu sync.RWMutex
	storage   StorageCallbacks
)

func storageSnapshot() StorageCallbacks {
	storageMu.RLock()
	defer storageMu.RUnlock()
	return storage
}

// RegisterStorageCallbacks installs the Go-side handlers for the custom
// piece storage. Pass a zero StorageCallbacks{} to uninstall — the next
// NewSession will then fall back to libtorrent's default disk_io.
//
// IMPORTANT: storage_callback installation is process-global; the next
// call to NewSession picks up the current setting. Already-running
// sessions keep whichever disk_io they were started with.
func RegisterStorageCallbacks(cb StorageCallbacks) error {
	empty := cb.Open == nil && cb.Close == nil && cb.Deleted == nil &&
		cb.Read == nil && cb.Write == nil && cb.Have == nil
	storageMu.Lock()
	storage = cb
	storageMu.Unlock()
	if empty {
		return codeToErr(C.tsl_uninstall_go_storage_callbacks())
	}
	return codeToErr(C.tsl_install_go_storage_callbacks())
}

// ----- //export trampolines invoked by lt_disk_io_trampolines.c -----
//
// Signatures match the `extern` decls in the trampoline file (`long long`
// is int64_t on every supported Linux platform; cgo emits the matching C
// type when we use C.longlong / C.int / C.uchar). Each one looks up the
// current Go-side StorageCallbacks under a read lock; if the relevant
// handler is nil the call is treated as "absent" (error / false).

//export tsl_storage_open_go
func tsl_storage_open_go(storage C.longlong, infoHash *C.uchar, numPieces C.int, pieceLength C.longlong) {
	cb := storageSnapshot()
	if cb.Open == nil {
		return
	}
	var h [20]byte
	src := unsafe.Slice((*byte)(unsafe.Pointer(infoHash)), 20)
	copy(h[:], src)
	cb.Open(int64(storage), h, int(numPieces), int64(pieceLength))
}

//export tsl_storage_close_go
func tsl_storage_close_go(storage C.longlong) {
	cb := storageSnapshot()
	if cb.Close != nil {
		cb.Close(int64(storage))
	}
}

//export tsl_storage_deleted_go
func tsl_storage_deleted_go(storage C.longlong) {
	cb := storageSnapshot()
	if cb.Deleted != nil {
		cb.Deleted(int64(storage))
	}
}

//export tsl_storage_read_go
func tsl_storage_read_go(storage C.longlong, piece C.int, offset C.longlong, buf *C.uchar, length C.int) C.int {
	if length <= 0 {
		return 0
	}
	cb := storageSnapshot()
	if cb.Read == nil {
		return -1
	}
	dst := unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(length))
	n, err := cb.Read(int64(storage), int(piece), int64(offset), dst)
	if err != nil {
		return -1
	}
	return C.int(n)
}

//export tsl_storage_write_go
func tsl_storage_write_go(storage C.longlong, piece C.int, offset C.longlong, buf *C.uchar, length C.int) C.int {
	if length <= 0 {
		return 0
	}
	cb := storageSnapshot()
	if cb.Write == nil {
		return -1
	}
	src := unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(length))
	n, err := cb.Write(int64(storage), int(piece), int64(offset), src)
	if err != nil {
		return -1
	}
	return C.int(n)
}

//export tsl_storage_have_go
func tsl_storage_have_go(storage C.longlong, piece C.int) C.int {
	cb := storageSnapshot()
	if cb.Have == nil {
		return 0
	}
	if cb.Have(int64(storage), int(piece)) {
		return 1
	}
	return 0
}
