package torr

import (
	"sync"
	"testing"

	"server/torr/state"
)

// A player opens a file with several requests at once while the UI polls the
// status, so GotInfo, Close and Status run concurrently on the same torrent.
// They all touch Stat, which must stay under t.mu — and a torrent closed while
// the metadata was awaited must never come back as working.
func TestTorrentStateTransitionsAreSynchronised(t *testing.T) {
	for round := 0; round < 50; round++ {
		tor := &Torrent{TorrentSpec: &TorrentSpec{}, closeCh: make(chan struct{})}

		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tor.GotInfo() // no libtorrent handle: fails and closes the torrent
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			tor.Close()
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = tor.Status()
		}()
		wg.Wait()

		tor.mu.Lock()
		stat := tor.Stat
		tor.mu.Unlock()
		if stat != state.TorrentClosed {
			t.Fatalf("round %d: torrent ended as %v, want closed", round, stat)
		}
	}
}
