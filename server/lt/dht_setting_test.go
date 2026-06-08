package lt

import "testing"

// TestSession_DHTMaxPeersApplied verifies that the dht_max_peers value we pass
// in SessionConfig (the knob behind the web "Max DHT connections" setting) is
// actually applied to the live libtorrent session — read back via SettingInt,
// proving setting_by_name recognises the name and the value is stored, not
// silently dropped.
func TestSession_DHTMaxPeersApplied(t *testing.T) {
	const want = 321
	s, err := NewSession(SessionConfig{
		"enable_dht":    false, // no network needed for this test
		"dht_max_peers": want,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	got, err := s.SettingInt("dht_max_peers")
	if err != nil {
		t.Fatalf("SettingInt(dht_max_peers): %v", err)
	}
	if got != want {
		t.Fatalf("dht_max_peers = %d, want %d", got, want)
	}

	// A bogus name must error (sanity-checks the recognition path).
	if _, err := s.SettingInt("nope_not_a_setting"); err == nil {
		t.Fatal("SettingInt(bogus) returned nil error, expected not-found")
	}
}
