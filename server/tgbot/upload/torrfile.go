package upload

import (
	"errors"
	"fmt"
	"path/filepath"

	"server/log"
	sets "server/settings"
	"server/tgbot/config"
	"server/torr"
	"server/torr/state"
)

var ERR_STOPPED = errors.New("stopped")

// TorrFile is the io.Reader the Telegram client uploads from. The
// reader plumbing is unwired in Etap 3 (Stream returns ErrNotImplemented),
// so the Read method currently returns ErrNotImplemented as well. The
// upload manager surfaces this to users until Etap 5 restores streaming.
type TorrFile struct {
	hash   string
	name   string
	wrk    *Worker
	offset int64
	size   int64
	id     int

	reader torr.Reader
}

func NewTorrFile(wrk *Worker, stFile *state.TorrentFileStat) (*TorrFile, error) {
	uid := int64(0)
	if wrk.c != nil && wrk.c.Sender() != nil {
		uid = wrk.c.Sender().ID
	}
	if config.Cfg != nil && config.Cfg.HostTG != "" && stFile.Length > 2*1024*1024*1024 {
		return nil, errors.New(tr(uid, "upload_file_too_large_2gb"))
	}
	if (config.Cfg == nil || config.Cfg.HostTG == "") && stFile.Length > 50*1024*1024 {
		return nil, errors.New(tr(uid, "upload_file_too_large_50mb"))
	}

	tf := new(TorrFile)
	tf.hash = wrk.torrentHash
	tf.name = filepath.Base(stFile.Path)
	tf.wrk = wrk
	tf.size = stFile.Length

	t := torr.GetTorrent(wrk.torrentHash)
	t.WaitInfo()

	files := t.Files()
	var file *torr.File
	for _, tfile := range files {
		if tfile.Path == stFile.Path {
			file = tfile
			break
		}
	}
	if file == nil {
		return nil, fmt.Errorf("file with id %v not found", stFile.Id)
	}
	if int64(sets.MaxSize) > 0 && file.Length > int64(sets.MaxSize) {
		log.TLogln("tg upload err size", file.Path, "max", sets.MaxSize)
		return nil, fmt.Errorf("file size exceeded max allowed %d bytes", sets.MaxSize)
	}

	reader := t.NewReader(file)
	if reader == nil {
		return nil, errors.New("cannot create torrent reader (streaming not implemented yet)")
	}
	tf.reader = reader
	return tf, nil
}

func (t *TorrFile) Read(p []byte) (n int, err error) {
	if t.wrk.isCancelled {
		return 0, ERR_STOPPED
	}
	if t.reader == nil {
		return 0, torr.ErrNotImplemented
	}
	n, err = t.reader.Read(p)
	t.offset += int64(n)
	return
}

func (t *TorrFile) Remaining() int64 {
	return t.size - t.offset
}

func (t *TorrFile) Close() {
	if t.reader != nil {
		t.reader.Close()
		t.reader = nil
	}
}
