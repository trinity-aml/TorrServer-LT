package torr

import (
	"net/http"
	"net/url"
	"testing"

	sets "server/settings"
)

// TestStreamGroupKey covers the per-device cache-window key resolution: the
// explicit session token wins, otherwise the client IP — recovered from a
// trusted proxy's X-Forwarded-For when present, and never from an untrusted one.
func TestStreamGroupKey(t *testing.T) {
	prev := sets.BTsets()
	t.Cleanup(func() { sets.StoreBTsets(prev) })

	mk := func(remote, xff, xreal, ss string) *http.Request {
		r := &http.Request{RemoteAddr: remote, Header: http.Header{}, URL: &url.URL{}}
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		if xreal != "" {
			r.Header.Set("X-Real-Ip", xreal)
		}
		if ss != "" {
			r.URL.RawQuery = "ss=" + ss
		}
		return r
	}

	cases := []struct {
		name    string
		trusted []string
		remote  string
		xff     string
		xreal   string
		ss      string
		wantKey string
	}{
		{
			name:    "session token wins over everything",
			remote:  "203.0.113.7:5000",
			xff:     "8.8.8.8",
			ss:      "abc123",
			wantKey: "ss:abc123",
		},
		{
			name:    "direct LAN client, no proxy header",
			remote:  "192.168.1.50:54000",
			wantKey: "ip:192.168.1.50",
		},
		{
			name:    "behind same-host proxy (default trust), XFF carries device",
			remote:  "127.0.0.1:8090",
			xff:     "192.168.1.51",
			wantKey: "ip:192.168.1.51",
		},
		{
			name:    "behind LAN proxy (default trust private), XFF carries device",
			remote:  "192.168.1.2:40000",
			xff:     "192.168.1.52",
			wantKey: "ip:192.168.1.52",
		},
		{
			name:    "untrusted public peer cannot spoof XFF",
			remote:  "203.0.113.9:443",
			xff:     "192.168.1.99",
			wantKey: "ip:203.0.113.9",
		},
		{
			name:    "proxy chain: first untrusted from the right is the client",
			remote:  "192.168.1.2:40000",
			xff:     "9.9.9.9, 10.0.0.5, 192.168.1.2",
			wantKey: "ip:9.9.9.9",
		},
		{
			name:    "X-Real-Ip fallback from a trusted proxy",
			remote:  "10.0.0.1:3128",
			xreal:   "10.0.0.77",
			wantKey: "ip:10.0.0.77",
		},
		{
			name:    "explicit trusted list: matching CIDR honours XFF",
			trusted: []string{"172.20.0.0/16"},
			remote:  "172.20.0.3:8000",
			xff:     "203.0.113.42",
			wantKey: "ip:203.0.113.42",
		},
		{
			name:    "explicit trusted list: private peer NOT listed is untrusted",
			trusted: []string{"172.20.0.0/16"},
			remote:  "192.168.1.2:40000",
			xff:     "203.0.113.42",
			wantKey: "ip:192.168.1.2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sets.StoreBTsets(&sets.BTSets{TrustedProxies: tc.trusted})
			got := streamGroupKey(mk(tc.remote, tc.xff, tc.xreal, tc.ss))
			if got != tc.wantKey {
				t.Fatalf("streamGroupKey = %q, want %q", got, tc.wantKey)
			}
		})
	}
}
