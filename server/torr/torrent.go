package torr

import (
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"

	"server/log"
	"server/lt"
	"server/settings"
	"server/torr/state"
	storageState "server/torr/storage/state"
	"server/torr/storage/torrstor"
	"server/torr/utils"
	"server/torrshash"
	utils2 "server/utils"
)

// Torrent is the public engine-agnostic wrapper. Composes a libtorrent
// handle with the metadata (title/poster/category) the rest of the
// project decorates torrents with.
type Torrent struct {
	Title    string
	Category string
	Poster   string
	Data     string
	*TorrentSpec

	Stat      state.TorrentStat
	Timestamp int64
	Size      int64

	bt *BTServer
	lh *lt.Torrent // libtorrent handle; nil while torrent is in DB only

	mu sync.Mutex

	// runtime metrics, updated by watch()
	lastTimeSpeed       time.Time
	DownloadSpeed       float64
	UploadSpeed         float64
	BytesReadUsefulData int64
	BytesWrittenData    int64

	// counters driven by the alert pump (atomic-friendly under mu)
	piecesDirtiedGood int64
	piecesDirtiedBad  int64

	PreloadSize    int64
	PreloadedBytes int64

	// play-gate state. The fill itself runs detached (context.Background) so an
	// impatient external player disconnecting mid-buffer can't abort it; the gate
	// tracks which file is filling and a channel that closes when it finishes, so
	// a reconnect for the SAME file WAITS for that same fill instead of skipping
	// the gate and streaming a half-buffer. preloadGateLast keeps the post-fill
	// debounce that suppresses the per-file preload storm a playlist client
	// triggers when it walks every &play entry of a multi-file torrent.
	preloadGateMu    sync.Mutex
	preloadGateIndex int
	preloadGateDone  chan struct{}
	preloadGateLast  time.Time

	// playStarted records that this torrent has begun playback once (on playStartIndex),
	// so the start-preload fires only at torrent START, not on every series switch: a
	// multi-file torrent is treated as a single unit. A play for a DIFFERENT file once
	// started streams directly (the reader's window buffers it), matching upstream
	// TorrServer, which preloads only on the explicit &preload — never on a play. Guarded
	// by preloadGateMu; reset implicitly when the torrent is dropped (fresh struct).
	playStarted    bool
	playStartIndex int

	DurationSeconds float64
	BitRate         string

	expiredTime time.Time

	gotInfoCh   chan struct{}
	gotInfoOnce sync.Once
	posterOnce  sync.Once

	closeCh   chan struct{}
	closeOnce sync.Once

	watcher *time.Ticker
}

// NewTorrent installs a new torrent in the session.
func NewTorrent(spec *TorrentSpec, bt *BTServer) (*Torrent, error) {
	if bt == nil || bt.session == nil {
		return nil, errors.New("torr.NewTorrent: BT client not connected")
	}
	if spec == nil {
		return nil, errors.New("torr.NewTorrent: nil spec")
	}

	// Trackers: applied via session-level retrackers settings in addition
	// to per-torrent. Mirror legacy RetrackersMode semantics.
	defTrackers := utils.GetDefTrackers()
	fileTrackers := utils.GetTrackerFromFile()
	switch settings.BTsets().RetrackersMode {
	case 1:
		spec.Trackers = append(spec.Trackers, defTrackers)
	case 2:
		spec.Trackers = nil
	case 3:
		spec.Trackers = [][]string{defTrackers}
	}
	if len(fileTrackers) > 0 {
		spec.Trackers = append(spec.Trackers, fileTrackers)
	}

	// If metadata is known at add time (InfoBytes present), scan the
	// per-piece cache dir for resume bits. Without metadata we don't
	// know the piece geometry yet; libtorrent will start downloading
	// and we'll discover have-state on subsequent restarts.
	var (
		havePieces []byte
		pieceCount int
	)
	if len(spec.InfoBytes) > 0 {
		if pt, err := lt.ParseTorrentBytes(spec.InfoBytes); err == nil && pt.HasMetadata && pt.NumPieces > 0 {
			pieceCount = pt.NumPieces
			havePieces = torrstor.ScanHavePieces(spec.InfoHash, pt.NumPieces, pt.PieceLength)
		}
	}

	// Hold the registry lock across check + session add + register. The
	// check-then-register used to be two separate critical sections, so two
	// concurrent adds of the same hash (e.g. an HTTP handler re-adding an
	// idle-dropped torrent racing GetTorrent's async promotion) both passed
	// the check and produced two Go Torrents over one libtorrent torrent.
	// The loser was orphaned — alerts route by hash to the registered
	// instance only — so its GotInfo timed out 90s later and Close()d the
	// shared lt torrent out from under an active stream.
	bt.mu.Lock()
	if existing := bt.torrents[spec.InfoHash]; existing != nil {
		if existing.Stat != state.TorrentClosed {
			bt.mu.Unlock()
			return existing, nil
		}
		// A closed instance left in the registry (e.g. a magnet whose metadata
		// fetch timed out: GotInfo's failure path Close()s without deregistering,
		// and expired() skips TorrentClosed so expireWatch never reaps it) must
		// not shadow the hash forever — every re-add would get the zombie back
		// and insta-fail. Its lt torrent is already removed; replace the entry.
		delete(bt.torrents, spec.InfoHash)
	}

	lh, err := bt.session.AddTorrent(lt.AddTorrentParams{
		Link:       magnetFromSpec(spec),
		InfoBytes:  spec.InfoBytes,
		Trackers:   spec.FlatTrackers(),
		SavePath:   legacySavePath(spec.InfoHash),
		Paused:     false,
		HavePieces: havePieces,
		PieceCount: pieceCount,
	})
	if err != nil {
		bt.mu.Unlock()
		return nil, err
	}

	timeout := torrentExpireTimeout()
	t := &Torrent{
		TorrentSpec:   spec,
		bt:            bt,
		lh:            lh,
		Stat:          state.TorrentAdded,
		Timestamp:     time.Now().Unix(),
		lastTimeSpeed: time.Now(),
		gotInfoCh:     make(chan struct{}),
		closeCh:       make(chan struct{}),
	}
	bt.torrents[spec.InfoHash] = t
	bt.mu.Unlock()

	t.AddExpiredTime(timeout)

	// BTsets.ConnectionsLimit is a per-torrent peer cap (anacrolix legacy);
	// the session-wide connections_limit is set separately in
	// buildSessionConfig. See lt_shim.h on lt_torrent_set_max_connections.
	if lh != nil && settings.BTsets() != nil && settings.BTsets().ConnectionsLimit > 0 {
		_ = lh.SetMaxConnections(settings.BTsets().ConnectionsLimit)
	}

	// If the .torrent payload was provided up-front, metadata is already
	// known — signal immediately. Otherwise this is a magnet and we must pull
	// the info-dict from peers before anything (file list, playlist, stream)
	// can proceed; kick an immediate DHT + tracker announce so peer discovery
	// for the metadata starts aggressively right away instead of waiting for a
	// Reader to attach (which only happens at playback start, long after the
	// playlist is requested). This is the difference between a magnet whose
	// playlist appears in a couple of seconds and one that times out.
	if len(spec.InfoBytes) > 0 {
		t.signalGotInfo()
	} else if lh != nil {
		go func() {
			_ = lh.ForceReannounce()
			if settings.BTsets() == nil || !settings.BTsets().DisableDHT {
				_ = lh.ForceDhtAnnounce()
			}
		}()
	}

	go t.watch()
	return t, nil
}

func magnetFromSpec(spec *TorrentSpec) string {
	if len(spec.InfoBytes) > 0 {
		return ""
	}
	return "magnet:?xt=urn:btih:" + spec.InfoHash.HexString()
}

func torrentExpireTimeout() time.Duration {
	t := time.Second * time.Duration(settings.BTsets().TorrentDisconnectTimeout)
	if t > time.Minute {
		t = time.Minute
	}
	return t
}

func legacySavePath(h Hash) string {
	if settings.BTsets() != nil && settings.BTsets().UseDisk && settings.BTsets().TorrentsSavePath != "" {
		return settings.BTsets().TorrentsSavePath + "/" + h.HexString()
	}
	return ""
}

// ----- metadata ready signalling -----

func (t *Torrent) signalGotInfo() {
	// Only fire once metadata is actually parsed. The add_torrent alert (and a
	// magnet's early alerts) arrive BEFORE the info-dict is known; consuming the
	// once here would burn it on a no-op SetAllPiecesPriority and then make the
	// real metadata_received alert a no-op — leaving the torrent at libtorrent's
	// default piece priority 1, which downloads the WHOLE torrent and thrashes
	// the bounded cache. So bail until metadata is ready and wait for a later
	// alert (metadata_received / torrent_finished) or WaitInfo's fast path.
	//
	// Snapshot the handle ONCE: Close() nils t.lh with no synchronization, and
	// an instance that loses a concurrent add race (remove → immediate re-add,
	// e.g. /gst/remove followed by the next playlist request) is Closed while
	// its NewTorrent goroutine is still in here — the re-read of t.lh between
	// the nil check and the call segfaulted on the nil receiver. A call on a
	// CLOSED handle is harmless (the C slot lookup returns an error).
	lh := t.lh
	if lh == nil {
		return
	}
	if have, _ := lh.HaveMetadata(); !have {
		return
	}
	t.gotInfoOnce.Do(func() {
		// Switch to lazy/streaming mode: download nothing until a Reader's
		// window or Preload bumps the specific pieces it needs.
		_ = lh.SetAllPiecesPriority(0)
		// Backfill the spec with the just-received info-dict: a magnet-added
		// torrent's spec has no InfoBytes, so without this every DB save stores
		// the bare magnet and every server restart re-fetches metadata from the
		// swarm (slow playlists / slow first play after restart).
		if t.TorrentSpec != nil && len(t.TorrentSpec.InfoBytes) == 0 {
			if mb, err := lh.Metadata(); err == nil && len(mb) > 0 {
				t.mu.Lock()
				t.TorrentSpec.InfoBytes = mb
				t.mu.Unlock()
			}
		}
		if t.gotInfoCh != nil {
			close(t.gotInfoCh)
		}
	})
}

// WaitInfo blocks until metadata is received or the torrent is closed or
// the configured timeout elapses. Returns true on success.
func (t *Torrent) WaitInfo() bool {
	if t == nil {
		return false
	}
	// Same handle snapshot as signalGotInfo: Close() nils t.lh concurrently.
	lh := t.lh
	if lh == nil {
		return false
	}
	// Fast path: already have metadata (e.g. info bytes were passed in).
	if have, _ := lh.HaveMetadata(); have {
		t.signalGotInfo()
		return true
	}
	deadline := time.Minute + time.Second*time.Duration(settings.BTsets().TorrentDisconnectTimeout)
	select {
	case <-t.gotInfoCh:
		return true
	case <-t.closeCh:
		return false
	case <-time.After(deadline):
		return false
	}
}

// GotInfo wraps WaitInfo with state transitions matching the legacy API.
func (t *Torrent) GotInfo() bool {
	if t == nil || t.Stat == state.TorrentClosed {
		return false
	}
	if t.Stat == state.TorrentPreload {
		return true
	}
	t.Stat = state.TorrentGettingInfo
	if t.WaitInfo() {
		t.Stat = state.TorrentWorking
		t.AddExpiredTime(torrentExpireTimeout())
		// Metadata (and so the release name) is now known — backfill a TMDB
		// poster for torrents that came in without one (bare magnets, tgbot,
		// autoload). No-op unless a TMDB API key is configured.
		go t.posterOnce.Do(t.fetchPosterIfMissing)
		return true
	}
	// Deregister before closing: expired() skips TorrentClosed, so a closed
	// instance left in the registry is never reaped and shadows its hash —
	// every later add of the same magnet returns the zombie and fails
	// instantly instead of retrying the metadata fetch.
	if t.bt != nil {
		t.bt.dropInstance(t)
	} else {
		t.Close()
	}
	return false
}

// AddExpiredTime pushes the auto-drop deadline forward.
func (t *Torrent) AddExpiredTime(d time.Duration) {
	newDeadline := time.Now().Add(d)
	t.mu.Lock()
	if t.expiredTime.Before(newDeadline) {
		t.expiredTime = newDeadline
	}
	t.mu.Unlock()
}

// expired reports whether the torrent has passed its auto-drop deadline and is
// safe to drop from the session: it must not be mid-handshake (getting metadata
// or preloading) or already closed, and must have no active reader. Playback,
// status polls and preload all push expiredTime forward, so an in-use torrent
// is never seen as expired. Used by BTServer.expireWatch.
func (t *Torrent) expired(now time.Time) bool {
	t.mu.Lock()
	stat := t.Stat
	deadline := t.expiredTime
	t.mu.Unlock()
	switch stat {
	case state.TorrentClosed, state.TorrentGettingInfo, state.TorrentPreload, state.TorrentInDB:
		return false
	}
	if deadline.IsZero() || now.Before(deadline) {
		return false
	}
	if c := torrstor.Global().CacheByHash([20]byte(t.Hash())); c != nil && c.ActiveReaders() > 0 {
		return false
	}
	return true
}

// ----- watch / progress -----

func (t *Torrent) watch() {
	t.watcher = time.NewTicker(time.Second)
	defer t.watcher.Stop()
	for {
		select {
		case <-t.closeCh:
			return
		case <-t.watcher.C:
			t.progressTick()
		}
	}
}

func (t *Torrent) progressTick() {
	if t.lh == nil {
		return
	}
	st, err := t.lh.Status()
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	dt := now.Sub(t.lastTimeSpeed).Seconds()
	if dt > 0 {
		dlDelta := st.TotalPayloadDownload - t.BytesReadUsefulData
		upDelta := st.TotalPayloadUpload - t.BytesWrittenData
		t.DownloadSpeed = float64(dlDelta) / dt
		t.UploadSpeed = float64(upDelta) / dt
	}
	t.BytesReadUsefulData = st.TotalPayloadDownload
	t.BytesWrittenData = st.TotalPayloadUpload
	t.lastTimeSpeed = now
}

// ----- shutdown -----

// Close pauses the underlying libtorrent torrent and marks it closed.
// Actual removal from the session is performed by BTServer.RemoveTorrent.
func (t *Torrent) Close() bool {
	if t == nil {
		return false
	}
	if t.Stat == state.TorrentClosed {
		return true
	}
	t.Stat = state.TorrentClosed
	t.markClosed()
	if t.lh != nil && t.bt != nil && t.bt.session != nil {
		// Only remove the libtorrent torrent if no OTHER live instance owns
		// it. A duplicate Torrent that lost an add race (or any stale copy)
		// shares the same underlying lt torrent with the registered one;
		// removing it here would kill an active stream. The registry entry
		// is either us or already deleted (RemoveTorrent deletes before
		// closing) — both mean we own the removal.
		if cur := t.bt.GetTorrent(t.Hash()); cur == nil || cur == t {
			_ = t.lh.Remove(false)
		}
	}
	t.lh = nil
	return true
}

func (t *Torrent) markClosed() {
	t.closeOnce.Do(func() {
		if t.closeCh != nil {
			close(t.closeCh)
		}
	})
}

// ----- accessors -----

// Hash returns the v1 info hash. Falls back to the spec if libtorrent
// handle is no longer valid.
func (t *Torrent) Hash() Hash {
	if t == nil {
		return Hash{}
	}
	if t.TorrentSpec != nil && !t.TorrentSpec.InfoHash.IsZero() {
		return t.TorrentSpec.InfoHash
	}
	if t.lh != nil {
		return NewHashFromHex(t.lh.InfoHash())
	}
	return Hash{}
}

// Name returns the torrent's display name (from metadata if available,
// otherwise from the spec's display_name / file name).
func (t *Torrent) Name() string {
	if t.lh != nil {
		if name := t.lh.DisplayName(); name != "" {
			return name
		}
	}
	if t.TorrentSpec != nil {
		return t.TorrentSpec.DisplayName
	}
	return ""
}

// Length returns the total payload size in bytes (0 before metadata).
func (t *Torrent) Length() int64 {
	if t == nil || t.lh == nil {
		return 0
	}
	return t.lh.TotalSize()
}

// Files returns the file list once metadata is known.
func (t *Torrent) Files() []*File {
	if t == nil || t.lh == nil {
		return nil
	}
	raw, err := t.lh.Files()
	if err != nil || len(raw) == 0 {
		return nil
	}
	out := make([]*File, 0, len(raw))
	for i := range raw {
		f := raw[i]
		out = append(out, &File{
			Index:  f.Index,
			Path:   f.Path,
			Length: f.Size,
			Offset: f.Offset,
		})
	}
	sort.Slice(out, func(i, j int) bool { return utils2.CompareStrings(out[i].Path, out[j].Path) })
	return out
}

// LTHandle returns the underlying libtorrent handle for callers that
// need to set piece priorities / deadlines. May be nil for DB-only
// torrents.
func (t *Torrent) LTHandle() *lt.Torrent { return t.lh }

// Status builds a state.TorrentStatus snapshot consumed by the web API.
func (t *Torrent) Status() *state.TorrentStatus {
	t.mu.Lock()
	defer t.mu.Unlock()

	st := new(state.TorrentStatus)
	st.Stat = t.Stat
	st.StatString = t.Stat.String()
	st.Title = t.Title
	st.Category = t.Category
	st.Poster = t.Poster
	st.Data = t.Data
	st.Timestamp = t.Timestamp
	st.TorrentSize = t.Size
	st.BitRate = t.BitRate
	st.DurationSeconds = t.DurationSeconds
	if t.TorrentSpec != nil {
		st.Hash = t.TorrentSpec.InfoHash.HexString()
	}

	if t.lh == nil {
		// DB-resident torrent (not in the session): recover the file list from
		// the record's cached Data so playlists and the web file tree work
		// without waking the torrent (= without a swarm metadata fetch).
		st.FileStats = fileStatsFromData(t.Data)
		return st
	}
	lst, err := t.lh.Status()
	if err != nil {
		return st
	}
	st.Name = lst.Name
	if st.Hash == "" {
		st.Hash = lst.InfoHash
	}
	st.LoadedSize = lst.TotalDone
	st.DownloadSpeed = t.DownloadSpeed
	st.UploadSpeed = t.UploadSpeed
	st.TotalPeers = lst.ListPeers
	st.PendingPeers = lst.ConnectCandidates
	st.ActivePeers = lst.NumPeers
	st.ConnectedSeeders = lst.NumSeeds
	st.HalfOpenPeers = 0
	st.BytesWritten = lst.TotalUpload
	st.BytesWrittenData = lst.TotalPayloadUpload
	st.BytesRead = lst.TotalDownload
	st.BytesReadData = lst.TotalPayloadDownload
	st.BytesReadUsefulData = lst.TotalPayloadDownload
	st.PreloadedBytes = t.PreloadedBytes
	st.PreloadSize = t.PreloadSize

	// libtorrent doesn't surface chunk counters directly via
	// torrent_status; approximate by dividing payload bytes by the
	// 16 KiB BitTorrent block size. Good enough for /cache + UI.
	const chunkSize = int64(16 * 1024)
	st.ChunksRead = lst.TotalPayloadDownload / chunkSize
	st.ChunksReadUseful = st.ChunksRead
	st.ChunksReadWasted = (lst.TotalDownload - lst.TotalPayloadDownload) / chunkSize
	st.ChunksWritten = lst.TotalPayloadUpload / chunkSize
	st.PiecesDirtiedGood = t.piecesDirtiedGood
	st.PiecesDirtiedBad = t.piecesDirtiedBad

	if !lst.HasMetadata {
		// Metadata still in flight — fall back to the DB-cached file list (if
		// this torrent was ever saved) so the UI isn't blank meanwhile.
		st.FileStats = fileStatsFromData(t.Data)
	} else {
		st.TorrentSize = lst.TotalSize
		st.FileStats = nil
		if files := t.Files(); len(files) > 0 {
			for _, f := range files {
				st.FileStats = append(st.FileStats, &state.TorrentFileStat{
					Id:     f.Index + 1, // legacy: 0 means undefined in the web UI
					Path:   f.Path,
					Length: f.Length,
				})
			}
			th := torrshash.New(st.Hash)
			th.AddField(torrshash.TagTitle, st.Title)
			th.AddField(torrshash.TagPoster, st.Poster)
			th.AddField(torrshash.TagCategory, st.Category)
			th.AddField(torrshash.TagSize, strconv.FormatInt(st.TorrentSize, 10))
			if t.TorrentSpec != nil && len(t.TorrentSpec.Trackers) > 0 {
				for _, tr := range t.TorrentSpec.Trackers[0] {
					th.AddField(torrshash.TagTracker, tr)
				}
			}
			if tok, err := torrshash.Pack(th); err == nil {
				st.TorrsHash = tok
			}
		}
	}
	return st
}

// ----- streaming / reader -----

// NewReader hands out an io.ReadSeekCloser over the requested file,
// backed by the Cache registered for this torrent. Returns nil if the
// cache hasn't been opened yet (metadata still in flight) or file is
// nil.
func (t *Torrent) NewReader(file *File) Reader {
	return t.NewReaderGroup(file, "")
}

// NewReaderGroup is NewReader with an explicit device/session key (the client IP)
// so concurrent streams of the same torrent from different devices each get their
// own isolated sliding window in the cache. The streaming HTTP path passes the
// caller's IP; internal consumers (DLNA index, tgbot, FUSE) use NewReader and
// share the default group.
func (t *Torrent) NewReaderGroup(file *File, group string) Reader {
	if t == nil || t.lh == nil || file == nil {
		return nil
	}
	cache := torrstor.Global().CacheByHash([20]byte(t.Hash()))
	if cache == nil {
		log.TLogln("torr.NewReader: no cache for", t.Hash().HexString())
		return nil
	}
	return torrstor.NewReader(cache, t.lh, torrstor.FileInfo{
		Index:  file.Index,
		Path:   file.Path,
		Offset: file.Offset,
		Length: file.Length,
	}, group)
}

// CloseReader releases a previously handed-out reader and pushes the
// torrent's auto-drop deadline forward.
func (t *Torrent) CloseReader(r Reader) {
	if r != nil {
		_ = r.Close()
	}
	t.AddExpiredTime(torrentExpireTimeout())
}

// CacheState returns a stubbed CacheState during Etap 3 — the real cache
// is reborn in Etap 4. Fields are filled enough to keep /cache and the
// tgbot snake command rendering something coherent (an empty piece map).
func (t *Torrent) CacheState() *storageState.CacheState {
	if t == nil {
		return &storageState.CacheState{Pieces: map[int]storageState.ItemState{}}
	}
	// Pull the real stats (Capacity, Filled, per-piece state) from the live
	// torrstor cache; fall back to an empty shell if it isn't open yet.
	var st *storageState.CacheState
	if c := torrstor.Global().CacheByHash([20]byte(t.Hash())); c != nil {
		st = c.State()
	} else {
		st = &storageState.CacheState{Pieces: map[int]storageState.ItemState{}}
	}
	st.Torrent = t.Status()
	if t.TorrentSpec != nil {
		st.Hash = t.TorrentSpec.InfoHash.HexString()
	}
	if t.lh != nil {
		st.PiecesCount = t.lh.NumPieces()
		st.PiecesLength = t.lh.PieceLength()
	}
	// Never hand the web UI a nil Pieces/Readers: useCreateCacheMap iterates both
	// without a null guard, so JSON null crashes the torrent info dialog.
	if st.Pieces == nil {
		st.Pieces = map[int]storageState.ItemState{}
	}
	if st.Readers == nil {
		st.Readers = []*storageState.ReaderState{}
	}
	return st
}
