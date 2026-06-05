package torrstor

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// DiskPiece persists a single piece to its own file on disk at
// `<savePath>/<infoHashHex>/<pieceID>`. Preserves the legacy
// piece-per-file layout so cache directories from the anacrolix era
// load unchanged.
//
// The file is lazily created on first write. ReadAt against a missing
// file returns io.EOF.
type DiskPiece struct {
	piece *Piece
	dir   string
	name  string

	mu sync.RWMutex
}

func newDiskPiece(p *Piece, savePath string) *DiskPiece {
	dir := filepath.Join(savePath, hashHex(p.cache.InfoHash))
	name := filepath.Join(dir, strconv.Itoa(p.Id))
	dp := &DiskPiece{piece: p, dir: dir, name: name}
	// Detect existing file from a previous run so the scan-resume path
	// reports a sane initial size.
	if fi, err := os.Stat(name); err == nil {
		p.size = fi.Size()
		if p.size >= p.cache.PieceLength {
			p.complete = true
		}
		p.accessed = fi.ModTime().Unix()
	}
	return dp
}

func (dp *DiskPiece) WriteAt(b []byte, off int64) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	dp.mu.Lock()
	defer dp.mu.Unlock()
	if err := os.MkdirAll(dp.dir, 0o777); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(dp.name, os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.WriteAt(b, off)
}

func (dp *DiskPiece) ReadAt(b []byte, off int64) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	dp.mu.RLock()
	defer dp.mu.RUnlock()
	f, err := os.OpenFile(dp.name, os.O_RDONLY, 0o666)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, io.EOF
		}
		return 0, err
	}
	defer f.Close()
	n, err := f.ReadAt(b, off)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

// Release removes the on-disk file.
func (dp *DiskPiece) Release() {
	dp.mu.Lock()
	defer dp.mu.Unlock()
	_ = os.Remove(dp.name)
}
