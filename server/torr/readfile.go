package torr

import (
	"context"
	"errors"
	"fmt"
	"io"

	"server/torr/state"
)

// ReadFile reads a small file of a torrent in full (external subtitles, for
// one). Unlike Stream it does not mark the file as viewed and writes no HTTP
// headers. A file larger than maxSize is refused, and reading stops when ctx
// is done.
func (t *Torrent) ReadFile(ctx context.Context, fileID int, maxSize int64) ([]byte, error) {
	if t == nil {
		return nil, errors.New("torr.ReadFile: no torrent")
	}
	if !t.GotInfo() {
		return nil, errors.New("torr.ReadFile: torrent has no metadata yet")
	}

	// Resolve the file the way Stream does: by Id in the status, then by path
	// among the torrent's files, so the ids match what the API hands out.
	var stFile *state.TorrentFileStat
	for _, fileStat := range t.Status().FileStats {
		if fileStat.Id == fileID {
			stFile = fileStat
			break
		}
	}
	if stFile == nil {
		return nil, fmt.Errorf("torr.ReadFile: file %d not found", fileID)
	}
	var file *File
	for _, f := range t.Files() {
		if f.Path == stFile.Path {
			file = f
			break
		}
	}
	if file == nil {
		return nil, fmt.Errorf("torr.ReadFile: file %d not found", fileID)
	}
	if file.Length > maxSize {
		return nil, fmt.Errorf("torr.ReadFile: file too large: %d bytes (max %d)", file.Length, maxSize)
	}

	reader := t.NewReader(file)
	if reader == nil {
		return nil, errors.New("torr.ReadFile: no reader (cache not yet open)")
	}
	defer t.CloseReader(reader)
	// Unblock a read parked on a missing piece as soon as the caller gives up.
	reader.SetContext(ctx)

	type result struct {
		data []byte
		err  error
	}
	// Buffered: the goroutine never blocks once we have left on ctx.
	done := make(chan result, 1)
	go func() {
		// Read exactly the file's length: the reader does not stop at the end
		// of the file and would carry on into the next one. (Stream is safe
		// because ServeContent copies exactly that many bytes.)
		buf := make([]byte, file.Length)
		_, err := io.ReadFull(reader, buf)
		done <- result{buf, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		return r.data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
