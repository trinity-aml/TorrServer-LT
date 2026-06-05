package torr

import (
	"net/http"
	"sync/atomic"
)

// activeStreams tracks the number of in-flight streaming clients. The
// counter is exposed via GetActiveStreams() so that web/tgbot can show
// it in their /server diagnostics.
var activeStreams int32

// Stream is a stub during Etap 3. The Reader path is wired up in Etap 5
// once the custom storage (Etap 4) is in place.
func (t *Torrent) Stream(fileID int, req *http.Request, resp http.ResponseWriter) error {
	_ = atomic.AddInt32(&activeStreams, 1)
	defer atomic.AddInt32(&activeStreams, -1)
	http.Error(resp, "streaming not implemented in this milestone", http.StatusServiceUnavailable)
	return ErrNotImplemented
}

// GetActiveStreams returns the current number of in-flight streams.
func GetActiveStreams() int32 {
	return atomic.LoadInt32(&activeStreams)
}
