//go:build gst

package gstreamer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestE2ETranscodeRealMKV runs the FULL GStreamer path on a real Matroska file:
// probe (real gst-discoverer) → pipeline (real GStreamer runtime, souphttpsrc)
// → init.mp4 + a transcoded fMP4 segment. Gated on GST_E2E_MKV (a path to a
// real .mkv) so it is skipped wherever no GStreamer runtime / sample exists.
//
//	GST_E2E_MKV=/path/to.mkv go test -tags gst -run E2E ./gstreamer/
func TestE2ETranscodeRealMKV(t *testing.T) {
	mkv := os.Getenv("GST_E2E_MKV")
	if mkv == "" {
		t.Skip("set GST_E2E_MKV=<path to .mkv> to run the real-transcode e2e")
	}
	if _, err := os.Stat(mkv); err != nil {
		t.Fatalf("GST_E2E_MKV not readable: %v", err)
	}
	conf := DefaultConfig()

	// 1) Probe through the real gst-discoverer on a file:// URL.
	probe, err := probeSource("file://"+mkv, conf)
	if err != nil {
		t.Fatalf("probeSource: %v", err)
	}
	var nVideo, nAudio int
	for _, tr := range probe.Tracks {
		switch tr.Type {
		case "video":
			nVideo++
		case "audio":
			nAudio++
		}
	}
	if nVideo < 1 || nAudio < 1 {
		t.Fatalf("probe found video=%d audio=%d, want >=1 each", nVideo, nAudio)
	}
	if !strings.Contains(strings.ToLower(probe.Container), "matroska") {
		t.Fatalf("probe container = %q, want matroska", probe.Container)
	}
	if got := resolveAudioIndex(probe, -1, ""); got < 0 {
		t.Fatalf("resolveAudioIndex(-1, \"\") = %d, want a real track", got)
	}

	// 2) Serve the file over HTTP (souphttpsrc reads it; ServeFile supports the
	//    Range requests the pipeline issues) and run the real pipeline.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, mkv)
	}))
	defer srv.Close()

	task := &Task{
		ID:              "e2e",
		SourceURL:       srv.URL,
		Probe:           probe,
		Config:          conf,
		LastSentSegment: -1,
		subtitleStores:  map[int]*subtitleStore{},
	}
	runner, err := newPipelineRunner(task, 0)
	if err != nil {
		t.Fatalf("newPipelineRunner: %v", err)
	}
	task.runner = runner
	defer runner.Dispose()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := task.EnsureInit(ctx, 0, 0); err != nil {
		t.Fatalf("EnsureInit: %v", err)
	}
	initLen := 0
	if err := task.WithInitMP4(func(b []byte) error { initLen = len(b); return nil }); err != nil {
		t.Fatalf("WithInitMP4: %v", err)
	}
	if initLen == 0 {
		t.Fatal("init.mp4 is empty")
	}

	segLen := 0
	if err := task.WithSegment(ctx, 0, 0, func(s Segment) error { segLen = s.Len(); return nil }); err != nil {
		t.Fatalf("WithSegment(0): %v", err)
	}
	if segLen == 0 {
		t.Fatal("segment 0 is empty")
	}

	t.Logf("E2E OK: container=%q video=%d audio=%d init.mp4=%dB seg0=%dB",
		probe.Container, nVideo, nAudio, initLen, segLen)
}
