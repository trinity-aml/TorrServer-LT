package torr

import (
	"context"
	"io"
)

// Reader is the streaming interface served by Torrent.NewReader. The
// concrete implementation lives in the storage package (Etap 4-5). For
// the engine swap milestone (Etap 3) every reader call returns nil and
// downstream consumers receive ErrNotImplemented from Stream.
type Reader interface {
	io.ReadSeekCloser
	SetReadahead(int64)
	Readahead() int64
	Offset() int64
	// SetContext attaches a cancellation context so a Read blocked on a piece
	// unblocks when the streaming client disconnects (e.g. the player seeks
	// away and aborts the old range request). Call before the first Read.
	SetContext(context.Context)
}
