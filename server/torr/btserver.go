package torr

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
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

	InitApiHelper(bt)
	return nil
}

// Disconnect stops the alert pump and tears down the session.
func (bt *BTServer) Disconnect() {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	if bt.session == nil {
		return
	}
	if bt.stopAlert != nil {
		close(bt.stopAlert)
		<-bt.alertDone
		bt.stopAlert = nil
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

func (bt *BTServer) registerTorrent(t *Torrent) {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	bt.torrents[t.Hash()] = t
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

func (bt *BTServer) handleAlert(a *lt.Alert) {
	if settings.BTsets != nil && settings.BTsets.EnableDebug && a.Type != "" {
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
	}
	// alert_mask is set to LT_ALERT_DEFAULT inside the shim — no need to
	// pass it here.

	if settings.BTsets == nil {
		return cfg, nil
	}
	s := settings.BTsets

	// DHT / PEX / LSD / UPNP / NATPMP toggles
	cfg["enable_dht"] = !s.DisableDHT
	cfg["enable_pex"] = !s.DisablePEX
	cfg["enable_lsd"] = s.EnableLPD
	cfg["enable_upnp"] = !s.DisableUPNP
	cfg["enable_natpmp"] = !s.DisableUPNP

	// TCP / uTP
	cfg["enable_outgoing_tcp"] = !s.DisableTCP
	cfg["enable_incoming_tcp"] = !s.DisableTCP
	cfg["enable_outgoing_utp"] = !s.DisableUTP
	cfg["enable_incoming_utp"] = !s.DisableUTP

	// Peer pool
	if s.ConnectionsLimit > 0 {
		cfg["connections_limit"] = s.ConnectionsLimit
		cfg["connections_slack"] = 10
	}

	// Bandwidth
	if s.DownloadRateLimit > 0 {
		cfg["download_rate_limit"] = s.DownloadRateLimit * 1024
	}
	if s.UploadRateLimit > 0 {
		cfg["upload_rate_limit"] = s.UploadRateLimit * 1024
	}

	// Encryption
	if s.ForceEncrypt {
		cfg["out_enc_policy"] = 0 // forced
		cfg["in_enc_policy"] = 0
		cfg["allowed_enc_level"] = 3 // rc4 only
	}

	// Listen port — libtorrent expects an interface CSV
	listenAddr := settings.TorAddr
	if listenAddr == "" {
		port := s.PeersListenPort
		if port <= 0 {
			port = 0 // libtorrent auto-selects
		}
		listenAddr = fmt.Sprintf("0.0.0.0:%d,[::]:%d", port, port)
	}
	cfg["listen_interfaces"] = listenAddr

	return cfg, nil
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
	} else if settings.BTsets != nil && settings.BTsets.EnableIPv6 {
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
	if settings.BTsets != nil && strings.TrimSpace(settings.BTsets.FriendlyName) != "" {
		return settings.BTsets.FriendlyName
	}
	return "TorrServer-LT/" + version.Version
}
