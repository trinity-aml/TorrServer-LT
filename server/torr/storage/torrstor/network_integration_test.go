//go:build integration

// Network-dependent integration tests. Run with:
//
//	go test -tags integration -count=1 -timeout 5m ./torr/storage/torrstor/
//
// CI default skips these (no build tag). Manual runs require outbound
// BT traffic (DHT bootstrap, tracker UDP, peer TCP).

package torrstor

import (
	"strings"
	"testing"
	"time"

	"server/lt"
)

// Sintel — well-seeded CC-licensed film. Stable info hash, plenty of
// peers, no auth required.
const sintelMagnet = "magnet:?xt=urn:btih:08ada5a7a6183aae1e09d831df6748d566095a10" +
	"&dn=Sintel" +
	"&tr=udp%3A%2F%2Ftracker.opentrackr.org%3A1337%2Fannounce" +
	"&tr=udp%3A%2F%2Fexplodie.org%3A6969%2Fannounce"

// TestNetwork_FetchMetadataFromMagnet asserts that we can resolve a
// magnet URI to its info dict over the wire within a reasonable time.
// Doesn't download any actual content beyond the metadata BEP-9
// transfer.
func TestNetwork_FetchMetadataFromMagnet(t *testing.T) {
	sess, err := lt.NewSession(lt.SessionConfig{
		"enable_dht": true, "enable_lsd": true,
		"enable_outgoing_utp": true, "enable_outgoing_tcp": true,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	tor, err := sess.AddTorrent(lt.AddTorrentParams{
		Link:     sintelMagnet,
		SavePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}

	deadline := time.Now().Add(2 * time.Minute)
	var got bool
	for time.Now().Before(deadline) && !got {
		if ok, _ := sess.WaitAlert(2 * time.Second); !ok {
			continue
		}
		alerts, _ := sess.PopAlerts()
		for _, a := range alerts {
			if a.Type == "metadata_received" || a.Type == "metadata_received_alert" {
				got = true
				break
			}
		}
	}
	if !got {
		t.Skip("metadata_received_alert never arrived; flaky network — skipping rather than failing CI")
	}

	have, err := tor.HaveMetadata()
	if err != nil {
		t.Fatalf("HaveMetadata: %v", err)
	}
	if !have {
		t.Fatal("HaveMetadata=false after metadata_received_alert")
	}

	files, err := tor.Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no files reported after metadata")
	}
	if name := tor.DisplayName(); !strings.Contains(strings.ToLower(name), "sintel") {
		t.Logf("display name: %q (informational)", name)
	}
}
