package torr

import (
	"encoding/json"

	"server/settings"
	"server/torr/state"
	"server/torr/utils"
)

// tsFiles is the optional cached file list embedded inside TorrentDB.Data
// for clients that prefetch file metadata without fully loading the
// torrent into the session.
type tsFiles struct {
	TorrServer struct {
		Files []*state.TorrentFileStat `json:"Files"`
	} `json:"TorrServer"`
}

// AddTorrentDB serialises a torrent record into the persistent DB.
func AddTorrentDB(torr *Torrent) {
	t := new(settings.TorrentDB)
	t.TorrentSpec = settingsSpec(torr.TorrentSpec)
	t.Title = torr.Title
	t.Category = torr.Category
	if torr.Data == "" {
		var f tsFiles
		f.TorrServer.Files = torr.Status().FileStats
		if buf, err := json.Marshal(&f); err == nil {
			t.Data = string(buf)
			torr.Data = t.Data
		}
	} else {
		t.Data = torr.Data
	}
	if torr.Poster != "" && utils.CheckImgUrl(torr.Poster) {
		t.Poster = torr.Poster
	}
	t.Size = torr.Size
	if t.Size == 0 {
		t.Size = torr.Length()
	}
	t.Timestamp = torr.Timestamp
	settings.AddTorrent(t)
}

// GetTorrentDB looks up a single torrent record by info hash.
func GetTorrentDB(hash Hash) *Torrent {
	for _, db := range settings.ListTorrent() {
		if db.TorrentSpec == nil {
			continue
		}
		if NewHashFromHex(db.TorrentSpec.InfoHash) == hash {
			return torrentFromDB(db)
		}
	}
	return nil
}

// RemTorrentDB removes a torrent record by info hash.
func RemTorrentDB(hash Hash) {
	settings.RemTorrent(hash.HexString())
}

// ListTorrentsDB returns the full DB content keyed by info hash.
func ListTorrentsDB() map[Hash]*Torrent {
	out := make(map[Hash]*Torrent)
	for _, db := range settings.ListTorrent() {
		if db.TorrentSpec == nil {
			continue
		}
		h := NewHashFromHex(db.TorrentSpec.InfoHash)
		out[h] = torrentFromDB(db)
	}
	return out
}

func torrentFromDB(db *settings.TorrentDB) *Torrent {
	if db == nil {
		return nil
	}
	t := new(Torrent)
	t.TorrentSpec = torrSpec(db.TorrentSpec)
	t.Title = db.Title
	t.Poster = db.Poster
	t.Category = db.Category
	t.Timestamp = db.Timestamp
	t.Size = db.Size
	t.Data = db.Data
	t.Stat = state.TorrentInDB
	return t
}

func settingsSpec(s *TorrentSpec) *settings.TorrentSpec {
	if s == nil {
		return nil
	}
	return &settings.TorrentSpec{
		InfoHash:    s.InfoHash.HexString(),
		InfoBytes:   s.InfoBytes,
		Trackers:    s.Trackers,
		DisplayName: s.DisplayName,
	}
}

func torrSpec(s *settings.TorrentSpec) *TorrentSpec {
	if s == nil {
		return nil
	}
	return &TorrentSpec{
		InfoHash:    NewHashFromHex(s.InfoHash),
		InfoBytes:   s.InfoBytes,
		Trackers:    s.Trackers,
		DisplayName: s.DisplayName,
	}
}
