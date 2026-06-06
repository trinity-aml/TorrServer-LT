package torr

import (
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"

	"server/lt"
	"server/log"
	"server/settings"
	"server/torr/state"
	storageState "server/torr/storage/state"
	"server/torr/storage/torrstor"
	"server/torr/utils"
	utils2 "server/utils"
	"server/torrshash"
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

	DurationSeconds float64
	BitRate         string

	expiredTime time.Time

	gotInfoCh chan struct{}
	gotInfoOnce sync.Once

	closeCh chan struct{}
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
	switch settings.BTsets.RetrackersMode {
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

	bt.mu.Lock()
	if existing := bt.torrents[spec.InfoHash]; existing != nil {
		bt.mu.Unlock()
		return existing, nil
	}
	bt.mu.Unlock()

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
	t.AddExpiredTime(timeout)

	// If the .torrent payload was provided up-front, metadata is already
	// known — signal immediately.
	if len(spec.InfoBytes) > 0 {
		t.signalGotInfo()
	}

	bt.registerTorrent(t)
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
	t := time.Second * time.Duration(settings.BTsets.TorrentDisconnectTimeout)
	if t > time.Minute {
		t = time.Minute
	}
	return t
}

func legacySavePath(h Hash) string {
	if settings.BTsets != nil && settings.BTsets.UseDisk && settings.BTsets.TorrentsSavePath != "" {
		return settings.BTsets.TorrentsSavePath + "/" + h.HexString()
	}
	return ""
}

// ----- metadata ready signalling -----

func (t *Torrent) signalGotInfo() {
	t.gotInfoOnce.Do(func() {
		// Switch to lazy/streaming mode: download nothing until a Reader's
		// window or Preload bumps the specific pieces it needs. Without this
		// libtorrent pulls the whole torrent (default piece priority 1),
		// thrashing the bounded cache on large files. No-op if metadata isn't
		// actually ready yet (returns an error we ignore).
		if t.lh != nil {
			_ = t.lh.SetAllPiecesPriority(0)
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
	if t.lh == nil {
		return false
	}
	// Fast path: already have metadata (e.g. info bytes were passed in).
	if have, _ := t.lh.HaveMetadata(); have {
		t.signalGotInfo()
		return true
	}
	deadline := time.Minute + time.Second*time.Duration(settings.BTsets.TorrentDisconnectTimeout)
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
		return true
	}
	t.Close()
	return false
}

// AddExpiredTime pushes the auto-drop deadline forward.
func (t *Torrent) AddExpiredTime(d time.Duration) {
	newDeadline := time.Now().Add(d)
	if t.expiredTime.Before(newDeadline) {
		t.expiredTime = newDeadline
	}
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
		_ = t.lh.Remove(false)
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

	if lst.HasMetadata {
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
	})
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
	return st
}
