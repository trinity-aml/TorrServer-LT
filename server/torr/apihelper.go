package torr

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"server/log"
	"server/lt"
	sets "server/settings"
	"server/version"
)

// bts is the package-level engine handle initialised by BTServer.Connect.
var bts *BTServer

// InitApiHelper is called by BTServer.Connect to publish the engine
// instance to the rest of this package.
func InitApiHelper(bt *BTServer) { bts = bt }

// LoadTorrent re-adds a DB-only torrent into the running session.
func LoadTorrent(tor *Torrent) *Torrent {
	if tor == nil || tor.TorrentSpec == nil {
		return nil
	}
	hadInfo := len(tor.TorrentSpec.InfoBytes) > 0
	out, err := NewTorrent(tor.TorrentSpec, bts)
	if err != nil {
		log.TLogln("torr.LoadTorrent:", err)
		return nil
	}
	if !out.WaitInfo() {
		return nil
	}
	out.Title = tor.Title
	out.Poster = tor.Poster
	out.Data = tor.Data
	out.Category = tor.Category
	if tor.Timestamp != 0 {
		out.Timestamp = tor.Timestamp
	}
	if tor.Size > 0 {
		out.Size = tor.Size
	}
	// A magnet-only DB record just produced its info-dict (backfilled into the
	// spec by signalGotInfo): persist it so the next server start serves this
	// torrent instantly instead of re-fetching metadata from the swarm.
	if !hadInfo && len(out.TorrentSpec.InfoBytes) > 0 && GetTorrentDB(out.Hash()) != nil {
		AddTorrentDB(out)
	}
	return out
}

// AddTorrent installs a torrent (creating or re-using the in-memory
// handle), backfilling user metadata from the DB if needed.
func AddTorrent(spec *TorrentSpec, title, poster, data, category string) (*Torrent, error) {
	dbt := GetTorrentDB(spec.InfoHash)

	// A spec parsed from a request link is usually hash-only; if the DB record
	// carries the info-dict, add with it so the torrent comes up with metadata
	// instantly instead of re-fetching it from the swarm (slow first play /
	// playlist after restart).
	if dbt != nil && dbt.TorrentSpec != nil && len(spec.InfoBytes) == 0 && len(dbt.TorrentSpec.InfoBytes) > 0 {
		spec.InfoBytes = dbt.TorrentSpec.InfoBytes
	}

	t, err := NewTorrent(spec, bts)
	if err != nil {
		log.TLogln("torr.AddTorrent:", err)
		return nil, err
	}

	if t.Title == "" {
		t.Title = title
		if title == "" && dbt != nil {
			t.Title = dbt.Title
		}
		if t.Title == "" && t.lh != nil {
			t.Title = t.Name()
		}
	}
	if t.Category == "" {
		t.Category = category
		if t.Category == "" && dbt != nil {
			t.Category = dbt.Category
		}
	}
	if t.Poster == "" {
		t.Poster = poster
		if t.Poster == "" && dbt != nil {
			t.Poster = dbt.Poster
		}
	}
	if t.Data == "" {
		t.Data = data
		if t.Data == "" && dbt != nil {
			t.Data = dbt.Data
		}
	}
	return t, nil
}

// SaveTorrentToDB persists a torrent record into config.db / json.
func SaveTorrentToDB(torr *Torrent) {
	if torr == nil {
		return
	}
	log.TLogln("torr.SaveTorrentToDB:", torr.Hash().HexString())
	AddTorrentDB(torr)
}

// GetTorrentInfo returns a torrent for read-only display — the live session
// instance if present, otherwise the DB record (cheap direct lookup) — WITHOUT
// promoting it into the session or extending its auto-drop deadline. For
// status/observation endpoints (e.g. the /cache piece-map dialog, which polls
// every 100ms): such polling must not keep a torrent resident after playback
// ends, but must still be able to render a dropped torrent's info (with an empty
// cache map) rather than 404. Only actual playback (an active reader) keeps a
// torrent alive. nil only when the hash is unknown to both session and DB.
func GetTorrentInfo(hashHex string) *Torrent {
	if bts != nil {
		if t := bts.GetTorrent(NewHashFromHex(hashHex)); t != nil {
			return t
		}
	}
	if dbt := sets.GetTorrent(hashHex); dbt != nil {
		return torrentFromDB(dbt)
	}
	return nil
}

// GetTorrent returns the in-memory torrent if present, or revives a
// DB-only one (asynchronously promoted to a live session torrent).
func GetTorrent(hashHex string) *Torrent {
	hash := NewHashFromHex(hashHex)
	timeout := torrentExpireTimeout()

	tor := bts.GetTorrent(hash)
	if tor != nil {
		tor.AddExpiredTime(timeout)
		return tor
	}
	dbt := GetTorrentDB(hash)
	if dbt == nil {
		return nil
	}
	tor = dbt
	go func() {
		log.TLogln("torr.GetTorrent: promoting DB torrent", tor.Hash().HexString())
		hadInfo := len(tor.TorrentSpec.InfoBytes) > 0
		fresh, err := NewTorrent(tor.TorrentSpec, bts)
		if err != nil || fresh == nil {
			log.TLogln("torr.GetTorrent: promote failed:", tor.Hash().HexString(), err)
			return
		}
		fresh.Title = tor.Title
		fresh.Poster = tor.Poster
		fresh.Data = tor.Data
		fresh.Size = tor.Size
		fresh.Timestamp = tor.Timestamp
		fresh.Category = tor.Category
		if fresh.GotInfo() && !hadInfo && len(fresh.TorrentSpec.InfoBytes) > 0 {
			// Magnet-only record just got its info-dict — persist it so future
			// server starts don't re-fetch metadata from the swarm.
			AddTorrentDB(fresh)
		}
	}()
	return tor
}

// SetTorrent updates the in-memory and DB-side metadata of a torrent.
func SetTorrent(hashHex, title, poster, category, data string) *Torrent {
	hash := NewHashFromHex(hashHex)
	tor := bts.GetTorrent(hash)
	dbt := GetTorrentDB(hash)

	if title == "" && tor == nil && dbt != nil {
		tor = GetTorrent(hashHex)
		if tor != nil {
			tor.GotInfo()
			if tor.lh != nil {
				title = tor.Name()
			}
		}
	}

	if tor != nil {
		if title == "" && tor.lh != nil {
			title = tor.Name()
		}
		tor.Title = title
		tor.Poster = poster
		tor.Category = category
		if data != "" {
			tor.Data = data
		}
	}
	if dbt != nil {
		dbt.Title = title
		dbt.Poster = poster
		dbt.Category = category
		if data != "" {
			dbt.Data = data
		}
		AddTorrentDB(dbt)
	}
	if tor != nil {
		return tor
	}
	return dbt
}

// RemTorrent removes a torrent from memory, the DB and (when configured)
// the on-disk cache directory.
func RemTorrent(hashHex string) {
	if sets.ReadOnly {
		log.TLogln("torr.RemTorrent: read-only DB mode:", hashHex)
		return
	}
	hash := NewHashFromHex(hashHex)

	tor := bts.GetTorrent(hash)
	if tor == nil {
		RemTorrentDB(hash)
		if sets.BTsets().UseDisk && hashHex != "" && hashHex != "/" {
			os.RemoveAll(filepath.Join(sets.BTsets().TorrentsSavePath, hashHex))
		}
		return
	}

	closeCh := tor.closeCh
	if bts.RemoveTorrent(hash) {
		select {
		case <-closeCh:
		case <-time.After(5 * time.Second):
			log.TLogln("torr.RemTorrent: timeout waiting for close:", hashHex)
		}
		if sets.BTsets().UseDisk && hashHex != "" && hashHex != "/" {
			name := filepath.Join(sets.BTsets().TorrentsSavePath, hashHex)
			if _, err := os.Stat(name); err == nil {
				log.TLogln("torr.RemTorrent: removing cache files for", hashHex)
				os.RemoveAll(name)
			}
		}
	}
	RemTorrentDB(hash)
}

// ListTorrent merges in-memory torrents with DB-only records.
func ListTorrent() []*Torrent {
	live := bts.ListTorrents()
	dbm := ListTorrentsDB()
	for h, t := range dbm {
		if _, ok := live[h]; !ok {
			live[h] = t
		}
	}
	out := make([]*Torrent, 0, len(live))
	for _, t := range live {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Timestamp != out[j].Timestamp {
			return out[i].Timestamp > out[j].Timestamp
		}
		return out[i].Title > out[j].Title
	})
	return out
}

// DropTorrent removes from the running session without touching the DB.
func DropTorrent(hashHex string) {
	bts.RemoveTorrent(NewHashFromHex(hashHex))
}

// SetSettings applies a new settings_pack and bounces the session.
func SetSettings(set *sets.BTSets) {
	if sets.ReadOnly {
		log.TLogln("torr.SetSettings: read-only DB mode")
		return
	}
	sets.SetBTSets(set)
	log.TLogln("torr.SetSettings: dropping all torrents")
	dropAllTorrent()
	time.Sleep(time.Second)
	log.TLogln("torr.SetSettings: disconnect")
	bts.Disconnect()
	log.TLogln("torr.SetSettings: reconnect")
	if err := bts.Connect(); err != nil {
		log.TLogln("torr.SetSettings: connect:", err)
	}
}

// SetDefSettings resets settings to defaults and bounces the session.
func SetDefSettings() {
	if sets.ReadOnly {
		log.TLogln("torr.SetDefSettings: read-only DB mode")
		return
	}
	sets.SetDefaultConfig()
	log.TLogln("torr.SetDefSettings: dropping all torrents")
	dropAllTorrent()
	time.Sleep(time.Second)
	bts.Disconnect()
	if err := bts.Connect(); err != nil {
		log.TLogln("torr.SetDefSettings: connect:", err)
	}
}

func dropAllTorrent() {
	for _, t := range bts.ListTorrents() {
		t.markClosed()
		if t.lh != nil {
			_ = t.lh.Remove(false)
		}
	}
}

// Shutdown closes the engine and the DB, then exits the process. Teardown is
// bounded: libtorrent session destruction can stall on tracker/DHT stop
// announces, and a wedged teardown must not leave a half-dead server that
// still answers HTTP but can never be stopped via the API.
func Shutdown() {
	done := make(chan struct{})
	go func() {
		bts.Disconnect()
		sets.CloseDB()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		log.TLogln("torr.Shutdown: teardown timed out — forcing exit")
	}
	log.TLogln("torr.Shutdown: received shutdown — quit")
	os.Exit(0)
}

// WriteStatus dumps server, per-torrent and libtorrent session stats to an
// io.Writer (the /stat page). Session counters are a fresh session_stats
// snapshot fetched through the alert pump (bounded wait, so /stat cannot
// hang); torrent details come from the same state the web UI uses.
func WriteStatus(w io.Writer) {
	if bts == nil || bts.session == nil {
		w.Write([]byte("session not running\n"))
		return
	}

	fmt.Fprintf(w, "TorrServer-LT %s (libtorrent %s)\n", version.Version, lt.Version())

	live := bts.ListTorrents()
	fmt.Fprintf(w, "Torrents: %d in session, %d in DB\n", len(live), len(ListTorrentsDB()))

	hashes := make([]Hash, 0, len(live))
	for h := range live {
		hashes = append(hashes, h)
	}
	sort.Slice(hashes, func(i, j int) bool {
		return live[hashes[i]].Timestamp < live[hashes[j]].Timestamp
	})
	for _, h := range hashes {
		t := live[h]
		st := t.Status()
		title := st.Title
		if title == "" {
			title = st.Name
		}
		fmt.Fprintf(w, "\n%s  %s\n", h.HexString(), title)
		fmt.Fprintf(w, "  status: %s, peers: %d/%d (active/known), seeders: %d\n",
			st.StatString, st.ActivePeers, st.TotalPeers, st.ConnectedSeeders)
		fmt.Fprintf(w, "  speed down/up: %.1f / %.1f KB/s, size: %d, preloaded: %d/%d\n",
			st.DownloadSpeed/1024, st.UploadSpeed/1024,
			st.TorrentSize, st.PreloadedBytes, st.PreloadSize)
		if cs := t.CacheState(); cs != nil {
			fmt.Fprintf(w, "  cache: %d/%d bytes (%d pieces), readers: %d\n",
				cs.Filled, cs.Capacity, cs.PiecesCount, len(cs.Readers))
		}
	}

	counters := bts.SessionStats(2 * time.Second)
	if len(counters) == 0 {
		w.Write([]byte("\nlibtorrent session counters: not available\n"))
		return
	}
	w.Write([]byte("\nlibtorrent session counters:\n"))
	names := make([]string, 0, len(counters))
	for name := range counters {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "  %s: %d\n", name, counters[name])
	}
}
