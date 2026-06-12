package torr

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/publicip"
	"github.com/wlynxg/anet"

	"server/lt"
	"server/settings"
	"server/torr/storage/torrstor"
	"server/torr/utils"
	"server/version"
)

// BTServer is the engine adapter. Owns the libtorrent session, the per-
// torrent registry and the single alert-pump goroutine. There is exactly
// one BTServer in the process; the abstraction is kept for parity with
// the previous code base.
type BTServer struct {
	mu        sync.Mutex
	session   *lt.Session
	torrents  map[Hash]*Torrent
	stopAlert chan struct{}
	alertDone chan struct{}
}

// NewBTS constructs an empty BTServer (no session yet — call Connect).
func NewBTS() *BTServer {
	return &BTServer{torrents: map[Hash]*Torrent{}}
}

// Connect builds the libtorrent session from current BTsets and starts
// the alert pump. Safe to call after Disconnect.
func (bt *BTServer) Connect() error {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	if bt.session != nil {
		return errors.New("torr.BTServer: already connected")
	}

	cfg, err := buildSessionConfig()
	if err != nil {
		return fmt.Errorf("torr.BTServer.Connect: %w", err)
	}

	// Activate the custom Go-backed disk_io before booting the session.
	// Already-running sessions (none in our model) would keep their
	// original disk_io; new sessions pick this up at session_new.
	if err := torrstor.Global().Install(); err != nil {
		return fmt.Errorf("torr.BTServer.Connect: install storage: %w", err)
	}

	s, err := lt.NewSession(cfg)
	if err != nil {
		_ = torrstor.Global().Uninstall()
		return fmt.Errorf("torr.BTServer.Connect: %w", err)
	}
	bt.session = s
	bt.torrents = map[Hash]*Torrent{}

	if filterText, _ := utils.ReadBlockedIPText(); filterText != "" {
		if err := s.SetIPFilter(filterText); err != nil {
			log.Println("torr.BTServer: SetIPFilter:", err)
		}
	}

	bt.stopAlert = make(chan struct{})
	bt.alertDone = make(chan struct{})
	go bt.alertPump(bt.stopAlert, bt.alertDone)
	go bt.expireWatch(bt.stopAlert)

	InitApiHelper(bt)
	return nil
}

// Disconnect stops the alert pump and tears down the session.
func (bt *BTServer) Disconnect() {
	// Stop the alert pump BEFORE taking bt.mu for teardown: handleAlert locks
	// bt.mu for its registry lookup, so holding the lock while waiting on
	// alertDone deadlocks whenever the pump is mid-batch — observed as
	// /shutdown hanging forever under steady alert traffic (DHT churn).
	bt.mu.Lock()
	stop, done := bt.stopAlert, bt.alertDone
	bt.stopAlert = nil
	bt.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}

	bt.mu.Lock()
	defer bt.mu.Unlock()
	if bt.session == nil {
		return
	}
	for _, t := range bt.torrents {
		t.markClosed()
	}
	bt.torrents = map[Hash]*Torrent{}
	_ = bt.session.Close()
	bt.session = nil
	// Drop the disk callbacks so subsequent NewSession calls (in tests
	// for instance) fall back to the default libtorrent disk_io.
	_ = torrstor.Global().Uninstall()
}

// Session is exposed so the rest of the package can call lt-level
// operations without re-implementing the wrapper.
func (bt *BTServer) Session() *lt.Session { return bt.session }

// GetTorrent returns the in-memory torrent for the given hash (nil if
// not currently registered with this server — note that database-only
// torrents are surfaced via apihelper.GetTorrent).
func (bt *BTServer) GetTorrent(h Hash) *Torrent {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	return bt.torrents[h]
}

// ListTorrents returns a snapshot of currently in-memory torrents.
func (bt *BTServer) ListTorrents() map[Hash]*Torrent {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	out := make(map[Hash]*Torrent, len(bt.torrents))
	for k, v := range bt.torrents {
		out[k] = v
	}
	return out
}

// RemoveTorrent removes a torrent from the session.
func (bt *BTServer) RemoveTorrent(h Hash) bool {
	bt.mu.Lock()
	t := bt.torrents[h]
	if t == nil {
		bt.mu.Unlock()
		return false
	}
	delete(bt.torrents, h)
	bt.mu.Unlock()
	return t.Close()
}

// dropInstance deregisters t — but only if t is still the instance registered
// for its hash — then closes it. Unlike RemoveTorrent (which operates by hash)
// it can never remove a different live Torrent that replaced t in the registry.
func (bt *BTServer) dropInstance(t *Torrent) {
	if t == nil {
		return
	}
	bt.mu.Lock()
	if bt.torrents[t.Hash()] == t {
		delete(bt.torrents, t.Hash())
	}
	bt.mu.Unlock()
	t.Close()
}

// alertPump pulls alerts from libtorrent and dispatches them to per-
// torrent notification channels. One goroutine per BTServer.
func (bt *BTServer) alertPump(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-stop:
			return
		default:
		}
		ok, err := bt.session.WaitAlert(500 * time.Millisecond)
		if err != nil {
			log.Println("torr.alertPump: WaitAlert:", err)
			time.Sleep(time.Second)
			continue
		}
		if !ok {
			continue
		}
		alerts, err := bt.session.PopAlerts()
		if err != nil {
			log.Println("torr.alertPump: PopAlerts:", err)
			continue
		}
		for i := range alerts {
			bt.handleAlert(&alerts[i])
		}
	}
}

// expireWatch drops idle torrents from the session once they pass their
// auto-drop deadline (TorrentDisconnectTimeout) with no active reader, which
// closes their cache and frees the RAM. Without it a torrent that was added or
// played once stays resident in the session forever — its cache never freed —
// since nothing else consumes expiredTime. Mirrors upstream TorrServer's
// inactivity drop; the DB record is kept, so the torrent is re-promoted on next
// access. Stops when the session's stopAlert channel is closed (Disconnect).
func (bt *BTServer) expireWatch(stop <-chan struct{}) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			now := time.Now()
			for _, t := range bt.ListTorrents() {
				if t == nil || !t.expired(now) {
					continue
				}
				log.Println("torr: auto-drop idle torrent", t.Hash().HexString())
				bt.RemoveTorrent(t.Hash())
			}
		}
	}
}

func (bt *BTServer) handleAlert(a *lt.Alert) {
	if settings.BTsets() != nil && settings.BTsets().EnableDebug && a.Type != "" {
		log.Printf("lt: %s — %s", a.Type, a.Message)
	}
	if a.TorrentHash == "" {
		return
	}
	bt.mu.Lock()
	t := bt.torrents[NewHashFromHex(a.TorrentHash)]
	bt.mu.Unlock()
	if t == nil {
		return
	}
	switch a.Type {
	case "metadata_received", "metadata_received_alert", "add_torrent":
		t.signalGotInfo()
	case "torrent_finished":
		t.signalGotInfo()
	case "torrent_error", "file_error":
		log.Printf("torr: torrent error for %s: %s", a.TorrentHash, a.Error)
	case "piece_finished", "piece_finished_alert", "read_piece":
		// Wake any Reader blocked on the piece. The actual byte data
		// is already on the disk side (libtorrent wrote it through
		// our cb_write before posting this alert).
		if cache := torrstor.Global().CacheByHash([20]byte(t.Hash())); cache != nil {
			cache.SignalPieceComplete(a.Piece)
		}
	}
}

// buildSessionConfig converts BTsets and CLI args into the JSON dict the
// shim hands to libtorrent's settings_pack. Settings are best-effort: any
// unrecognised key is silently ignored on the C++ side.
func buildSessionConfig() (lt.SessionConfig, error) {
	cfg := lt.SessionConfig{
		"user_agent":       "qBittorrent/4.3.9",
		"peer_fingerprint": "-qB4390-",

		// Streaming-tuned defaults (cf. elgatito/elementum). These trade some
		// bandwidth politeness for fast start/seek: find peers quickly, keep
		// deep request pipelines, and race the last buffer pieces via end-game
		// mode. BTsets-derived keys below may override where they overlap.
		//
		// NB on request pipelining: keep request_queue_time / max_out_request_queue
		// near libtorrent's defaults. Cranking them (e.g. queue_time=2s,
		// max_out=5000) makes us flood a single peer with hundreds–thousands of
		// outstanding block requests; many clients (qBittorrent) cap incoming
		// requests and choke/drop a peer that over-asks, which *slows* streaming
		// and can stall it. The default-ish values below pipeline enough for
		// throughput without tripping peer anti-flood limits.
		"strict_end_game_mode":         true,  // fetch the final buffer pieces from many peers
		"prioritize_partial_pieces":    false, // prefer finishing the reader's window in order
		"announce_to_all_tiers":        true,  // hit every tracker tier for peers fast
		"announce_to_all_trackers":     true,
		"min_announce_interval":        30,
		"max_suggest_pieces":           50,
		"request_queue_time":           3,    // libtorrent default; ~3s of requests in flight
		"max_out_request_queue":        1500, // cap outstanding block requests per peer
		"max_allowed_in_request_queue": 2000, // libtorrent default
		"send_buffer_watermark":        500 * 1024,
		"send_buffer_watermark_factor": 50,
		"connection_speed":             250, // outgoing connection attempts per second (default 30)
		"max_peerlist_size":            50000,
		"max_pex_peers":                200,
		"dht_upload_rate_limit":        50000,
		"mixed_mode_algorithm":         0, // prefer_tcp: steadier throughput for streaming

		// Swarm ramp-up: connect to many peers immediately and fail the dead
		// ones fast, so the first seconds of playback see the full swarm
		// instead of trickling in at libtorrent's polite defaults.
		"torrent_connect_boost":             100, // peers to try the moment a torrent is added (default 30)
		"peer_connect_timeout":              7,   // give up on unreachable peers sooner (default 15)
		"piece_timeout":                     10,  // re-request a slow block from another peer sooner (default 20)
		"min_reconnect_time":                10,  // retry failed peers sooner (default 60)
		"allow_multiple_connections_per_ip": true,
		"dht_announce_interval":             60,   // keep fresh peers flowing in (default 15 min)
		"auto_scrape_interval":              1200, // cf. elementum
		"auto_scrape_min_interval":          900,
		"stop_tracker_timeout":              1,
		"seed_choking_algorithm":            1, // fastest_upload: reciprocity favours fast peers

		// Don't let the queue manager pause our stream. Torrents are added
		// auto_managed, and libtorrent's default active limits (≈3 downloads /
		// 15 active) would queue+pause a torrent mid-playback once a few are
		// running. Lift every active limit to unlimited (cf. elementum) so the
		// auto-manager never stops a torrent someone is streaming.
		"active_downloads":            -1,
		"active_seeds":                -1,
		"active_limit":                -1,
		"active_tracker_limit":        -1,
		"active_dht_limit":            -1,
		"active_lsd_limit":            -1,
		"rate_limit_ip_overhead":      false, // don't count protocol overhead against rate caps
		"send_buffer_low_watermark":   10 * 1024,
		"close_redundant_connections": false, // keep peers around for seek re-requests
	}
	// alert_mask is set to LT_ALERT_DEFAULT inside the shim — no need to
	// pass it here.

	if settings.BTsets() == nil {
		return cfg, nil
	}
	s := settings.BTsets()

	// End-game: race the final buffer pieces across peers (on by default).
	cfg["strict_end_game_mode"] = !s.DisableEndGame

	// DHT / PEX / LSD / UPNP / NATPMP toggles
	cfg["enable_dht"] = !s.DisableDHT
	if s.DHTConnectionsLimit > 0 {
		cfg["dht_max_peers"] = s.DHTConnectionsLimit
	}
	cfg["enable_pex"] = !s.DisablePEX
	cfg["enable_lsd"] = s.EnableLPD
	cfg["enable_upnp"] = !s.DisableUPNP
	cfg["enable_natpmp"] = !s.DisableUPNP

	// TCP / uTP
	cfg["enable_outgoing_tcp"] = !s.DisableTCP
	cfg["enable_incoming_tcp"] = !s.DisableTCP
	cfg["enable_outgoing_utp"] = !s.DisableUTP
	cfg["enable_incoming_utp"] = !s.DisableUTP

	// Peer pool. BTsets.ConnectionsLimit is historically PER-TORRENT (anacrolix
	// EstablishedConnsPerTorrent, default 25) and is applied per torrent via
	// torrent_handle::set_max_connections on add. settings_pack's
	// connections_limit is the SESSION-WIDE cap (libtorrent default 200) —
	// mapping the per-torrent value straight into it used to throttle the whole
	// session to 25 peers, the single biggest speed killer after the LT port.
	// Keep the session cap comfortably above a few concurrent torrents' worth.
	if s.ConnectionsLimit > 0 {
		sessionConns := s.ConnectionsLimit * 4
		if sessionConns < 200 {
			sessionConns = 200
		}
		cfg["connections_limit"] = sessionConns
		cfg["connections_slack"] = 10
	}

	// Bandwidth
	if s.DownloadRateLimit > 0 {
		cfg["download_rate_limit"] = s.DownloadRateLimit * 1024
	}
	if s.UploadRateLimit > 0 {
		cfg["upload_rate_limit"] = s.UploadRateLimit * 1024
	}

	// Leech-only: never unchoke a peer, so we don't upload at all. Overrides
	// the active-limit seeding restored above. (Eviction no longer depends on
	// this — WeDontHave keeps the have-bitfield honest — so it's purely the
	// user's ratio choice now.)
	if s.DisableUpload {
		cfg["unchoke_slots_limit"] = 0
	}

	// Encryption
	if s.ForceEncrypt {
		cfg["out_enc_policy"] = 0 // forced
		cfg["in_enc_policy"] = 0
		cfg["allowed_enc_level"] = 3 // rc4 only
	}

	// Listen port — libtorrent expects an interface CSV. We always
	// include the v4 wildcard; v6 only when explicitly enabled to
	// match the BTsets toggle.
	listenAddr := settings.TorAddr
	if listenAddr == "" {
		port := s.PeersListenPort
		if port < 0 {
			port = 0 // libtorrent auto-selects
		}
		listenAddr = fmt.Sprintf("0.0.0.0:%d", port)
		if s.EnableIPv6 {
			listenAddr += fmt.Sprintf(",[::]:%d", port)
		}
	}
	cfg["listen_interfaces"] = listenAddr

	// Proxy (if CLI --proxy-url is set, plumb it through). Honours the
	// --proxy-mode flag (tracker / peers / full).
	applyProxyConfig(cfg)

	return cfg, nil
}

// applyProxyConfig translates settings.Args.ProxyURL + ProxyMode into
// libtorrent's settings_pack proxy_* keys. No-op when ProxyURL is empty.
//
// Schemes recognised:
//
//	http / https → http proxy (libtorrent's proxy_type=5, with auth = 6)
//	socks4 / socks4a → SOCKS4
//	socks5 / socks5h → SOCKS5 (5_pw when user/password present)
func applyProxyConfig(cfg lt.SessionConfig) {
	if settings.Args == nil || settings.Args.ProxyURL == "" {
		return
	}
	u, err := url.Parse(settings.Args.ProxyURL)
	if err != nil || u.Host == "" {
		log.Println("torr: cannot parse proxy URL:", err)
		return
	}

	scheme := strings.ToLower(u.Scheme)
	username := ""
	password := ""
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
	}
	hasAuth := username != ""

	// proxy_type enum values mirror libtorrent's settings_pack::proxy_type_t.
	const (
		ptNone     = 0
		ptSOCKS4   = 1
		ptSOCKS5   = 2
		ptSOCKS5pw = 3
		ptHTTP     = 4
		ptHTTPpw   = 5
		ptI2P      = 6
	)

	var proxyType int
	resolveViaProxy := false
	switch scheme {
	case "http", "https":
		if hasAuth {
			proxyType = ptHTTPpw
		} else {
			proxyType = ptHTTP
		}
	case "socks4", "socks4a":
		proxyType = ptSOCKS4
		resolveViaProxy = scheme == "socks4a"
	case "socks5":
		if hasAuth {
			proxyType = ptSOCKS5pw
		} else {
			proxyType = ptSOCKS5
		}
	case "socks5h":
		if hasAuth {
			proxyType = ptSOCKS5pw
		} else {
			proxyType = ptSOCKS5
		}
		resolveViaProxy = true
	default:
		log.Println("torr: unsupported proxy scheme:", scheme)
		return
	}

	host := u.Hostname()
	port := 0
	if p := u.Port(); p != "" {
		fmt.Sscanf(p, "%d", &port)
	}

	cfg["proxy_type"] = proxyType
	cfg["proxy_hostname"] = host
	cfg["proxy_port"] = port
	cfg["proxy_username"] = username
	cfg["proxy_password"] = password
	cfg["proxy_hostnames"] = resolveViaProxy

	// ProxyMode decides which connections go through the proxy.
	tracker, peer := false, false
	switch settings.Args.ProxyMode {
	case "tracker", "":
		tracker = true
	case "peers":
		peer = true
	case "full":
		tracker = true
		peer = true
	default:
		log.Println("torr: unknown proxy mode, defaulting to tracker:", settings.Args.ProxyMode)
		tracker = true
	}
	cfg["proxy_tracker_connections"] = tracker
	cfg["proxy_peer_connections"] = peer
}

// ReloadIPFilter re-reads bip.txt/wip.txt from disk and pushes the new
// filter into the live session. Safe no-op when no session is running.
func (bt *BTServer) ReloadIPFilter() error {
	bt.mu.Lock()
	s := bt.session
	bt.mu.Unlock()
	if s == nil {
		return nil
	}
	text, err := utils.ReadBlockedIPText()
	if err != nil {
		return err
	}
	return s.SetIPFilter(text)
}

// publicIPs is exposed for legacy callers; libtorrent itself learns its
// public IP via UPnP/STUN/tracker reports, so we don't push these into
// settings — but keep the helpers around for stat pages and tests.
func publicIPs(ctx context.Context) (v4, v6 net.IP) {
	if settings.PubIPv4 != "" {
		v4 = net.ParseIP(settings.PubIPv4)
	} else if ip, err := publicip.Get4(ctx); err == nil {
		v4 = ip
	}
	if settings.PubIPv6 != "" {
		v6 = net.ParseIP(settings.PubIPv6)
	} else if settings.BTsets() != nil && settings.BTsets().EnableIPv6 {
		if ip, err := publicip.Get6(ctx); err == nil {
			v6 = ip
		}
	}
	return
}

// GetLocalIPs is a host helper used by web/server.go and a few other
// places. Kept here so removing dependence on anet from those callers
// stays simple. Returns IPv4/IPv6 addresses of every up, non-loopback
// interface.
func GetLocalIPs() []string {
	ifaces, err := anet.Interfaces()
	if err != nil {
		return nil
	}
	var list []string
	for _, i := range ifaces {
		addrs, _ := anet.InterfaceAddrsByInterface(&i)
		if i.Flags&net.FlagUp == 0 {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() {
				list = append(list, ip.String())
			}
		}
	}
	return list
}

// Version returns the TorrServer-LT build tag (mirrors version.Version)
// for consumers that don't want to import the version package directly.
func Version() string { return version.Version }

// UserAgent returns the wire UA libtorrent reports to peers/trackers.
func UserAgent() string {
	if settings.BTsets() != nil && strings.TrimSpace(settings.BTsets().FriendlyName) != "" {
		return settings.BTsets().FriendlyName
	}
	return "TorrServer-LT/" + version.Version
}
