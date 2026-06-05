package torr

import (
	"testing"

	"server/lt"
	"server/settings"
)

// withArgs temporarily swaps settings.Args. Returns a teardown for t.Cleanup.
func withArgs(t *testing.T, a settings.ExecArgs) {
	t.Helper()
	prev := settings.Args
	settings.Args = &a
	t.Cleanup(func() { settings.Args = prev })
}

func TestApplyProxyConfig_NoURL(t *testing.T) {
	withArgs(t, settings.ExecArgs{})
	cfg := lt.SessionConfig{}
	applyProxyConfig(cfg)
	if _, ok := cfg["proxy_type"]; ok {
		t.Fatalf("expected no proxy keys when ProxyURL is empty, got %v", cfg)
	}
}

func TestApplyProxyConfig_HTTPWithoutAuth(t *testing.T) {
	withArgs(t, settings.ExecArgs{ProxyURL: "http://10.0.0.1:8888", ProxyMode: "tracker"})
	cfg := lt.SessionConfig{}
	applyProxyConfig(cfg)
	if cfg["proxy_type"] != 4 {
		t.Fatalf("proxy_type: got %v, want 4 (http)", cfg["proxy_type"])
	}
	if cfg["proxy_hostname"] != "10.0.0.1" {
		t.Fatalf("proxy_hostname: %v", cfg["proxy_hostname"])
	}
	if cfg["proxy_port"] != 8888 {
		t.Fatalf("proxy_port: %v", cfg["proxy_port"])
	}
	if cfg["proxy_tracker_connections"] != true {
		t.Fatal("tracker_connections should be true")
	}
	if cfg["proxy_peer_connections"] != false {
		t.Fatal("peer_connections should be false in tracker mode")
	}
}

func TestApplyProxyConfig_SOCKS5WithAuth(t *testing.T) {
	withArgs(t, settings.ExecArgs{
		ProxyURL:  "socks5://alice:secret@proxy.example:1080",
		ProxyMode: "full",
	})
	cfg := lt.SessionConfig{}
	applyProxyConfig(cfg)
	if cfg["proxy_type"] != 3 { // socks5_pw
		t.Fatalf("proxy_type: got %v, want 3", cfg["proxy_type"])
	}
	if cfg["proxy_username"] != "alice" || cfg["proxy_password"] != "secret" {
		t.Fatalf("creds: %v / %v", cfg["proxy_username"], cfg["proxy_password"])
	}
	if cfg["proxy_tracker_connections"] != true || cfg["proxy_peer_connections"] != true {
		t.Fatal("full mode should enable both tracker and peer")
	}
}

func TestApplyProxyConfig_SOCKS5H_ResolvesViaProxy(t *testing.T) {
	withArgs(t, settings.ExecArgs{ProxyURL: "socks5h://proxy:1080", ProxyMode: "peers"})
	cfg := lt.SessionConfig{}
	applyProxyConfig(cfg)
	if cfg["proxy_type"] != 2 {
		t.Fatalf("proxy_type: got %v, want 2 (socks5)", cfg["proxy_type"])
	}
	if cfg["proxy_hostnames"] != true {
		t.Fatal("socks5h should set proxy_hostnames=true")
	}
	if cfg["proxy_peer_connections"] != true || cfg["proxy_tracker_connections"] != false {
		t.Fatal("peers mode wiring")
	}
}

func TestApplyProxyConfig_UnsupportedScheme(t *testing.T) {
	withArgs(t, settings.ExecArgs{ProxyURL: "ftp://proxy:21", ProxyMode: "tracker"})
	cfg := lt.SessionConfig{}
	applyProxyConfig(cfg)
	if _, ok := cfg["proxy_type"]; ok {
		t.Fatalf("unsupported scheme should be ignored, got %v", cfg)
	}
}

func TestApplyProxyConfig_SOCKS4A_ResolvesViaProxy(t *testing.T) {
	withArgs(t, settings.ExecArgs{ProxyURL: "socks4a://proxy:1080", ProxyMode: ""})
	cfg := lt.SessionConfig{}
	applyProxyConfig(cfg)
	if cfg["proxy_type"] != 1 {
		t.Fatalf("proxy_type: got %v, want 1 (socks4)", cfg["proxy_type"])
	}
	if cfg["proxy_hostnames"] != true {
		t.Fatal("socks4a should set proxy_hostnames=true")
	}
	// default mode = tracker
	if cfg["proxy_tracker_connections"] != true {
		t.Fatal("empty mode should default to tracker")
	}
}
