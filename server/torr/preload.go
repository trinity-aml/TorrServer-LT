package torr

import (
	"server/log"
	"server/settings"
)

// Preload (Torrent method) is a stub until Etap 6 plumbs piece
// priorities/deadlines through the new reader. Documented so callers
// (web/api/stream.go and the upcoming /preload endpoint) keep working.
func (t *Torrent) Preload(index int, size int64) {
	log.TLogln("torr.Preload: stubbed in this milestone")
}

// Preload (free function) keeps API parity with the legacy code that
// did `torr.Preload(tor, index)` at the call site.
func Preload(torr *Torrent, index int) {
	if torr == nil || settings.BTsets == nil {
		return
	}
	cache := float32(settings.BTsets.CacheSize)
	prc := float32(settings.BTsets.PreloadCache)
	size := int64((cache / 100.0) * prc)
	if size <= 0 {
		return
	}
	torr.Preload(index, size)
}
