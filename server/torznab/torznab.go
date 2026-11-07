package torznab

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"server/log"
	"server/rutor/models"
	"server/settings"
	"server/version"
)

const httpTimeout = 30 * time.Second

var httpClient = &http.Client{Timeout: httpTimeout}

// userAgent is sent with every request: some indexers sit behind a WAF that
// answers 403 to a client without one.
var userAgent = "TorrServer-Torznab/" + version.Version

func newRequest(ctx context.Context, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	return req, nil
}

type TorznabAttribute struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type TorznabEnclosure struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

type TorznabItem struct {
	Title       string             `xml:"title"`
	Link        string             `xml:"link"`
	Description string             `xml:"description"`
	PubDate     string             `xml:"pubDate"`
	Size        int64              `xml:"size"`
	Jackett     string             `xml:"jackettindexer"`  // source tracker behind a Jackett aggregate
	Prowlarr    string             `xml:"prowlarrindexer"` // same for Prowlarr
	Enclosure   []TorznabEnclosure `xml:"enclosure"`
	Attributes  []TorznabAttribute `xml:"attr"`
}

type TorznabChannel struct {
	Items []TorznabItem `xml:"item"`
}

type TorznabResponse struct {
	Channel TorznabChannel `xml:"channel"`
}

type torznabError struct {
	XMLName     xml.Name
	Code        string `xml:"code,attr"`
	Description string `xml:"description,attr"`
}

type CapsCategory struct {
	ID      string         `xml:"id,attr"`
	Name    string         `xml:"name,attr"`
	Subcats []CapsCategory `xml:"subcat"`
}

type CapsLimits struct {
	Max     int `xml:"max,attr"`
	Default int `xml:"default,attr"`
}

type Caps struct {
	XMLName    xml.Name       `xml:"caps"`
	Limits     CapsLimits     `xml:"limits"`
	Categories []CapsCategory `xml:"categories>category"`
}

// normalizeHost turns a configured Torznab base into an API URL ending in /api.
// Paths that already end with /api or /api/ are left as-is (minus trailing slash handling).
func normalizeHost(host string) (*url.URL, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, fmt.Errorf("empty host")
	}
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = "http://" + host
	}

	u, err := url.Parse(host)
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid host: %s", host)
	}

	path := strings.TrimSuffix(u.Path, "/")
	if strings.HasSuffix(strings.ToLower(path), "/api") || strings.EqualFold(path, "/api") || strings.EqualFold(path, "api") {
		u.Path = path
		u.RawQuery = ""
		u.Fragment = ""
		return u, nil
	}

	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	u.Path += "api"
	u.RawQuery = ""
	u.Fragment = ""
	return u, nil
}

// Search runs a query against one configured indexer (index >= 0) or all of
// them (index < 0). cat pins the Torznab categories for this one search: nil
// means "whatever each indexer is configured for", and a non-nil empty string
// means "no categories at all".
func Search(ctx context.Context, query string, index int, cat *string, offset, limit int) []*models.TorrentDetails {
	if ctx == nil {
		ctx = context.Background()
	}
	if !settings.BTsets().EnableTorznabSearch || len(settings.BTsets().TorznabUrls) == 0 {
		return nil
	}

	searchOffset := offset
	if index < 0 {
		searchOffset = 0
	}

	var allResults []*models.TorrentDetails
	if index >= 0 && index < len(settings.BTsets().TorznabUrls) {
		config := settings.BTsets().TorznabUrls[index]
		if config.Host != "" && config.Key != "" {
			return searchOne(ctx, config.Host, config.Key, query, indexerLabel(config), searchCategories(config, cat), searchOffset, limit)
		}
		return nil
	}

	for _, config := range settings.BTsets().TorznabUrls {
		if config.Host == "" || config.Key == "" {
			continue
		}
		results := searchOne(ctx, config.Host, config.Key, query, indexerLabel(config), searchCategories(config, cat), searchOffset, limit)
		if results != nil {
			allResults = append(allResults, results...)
		}
	}
	return allResults
}

// itemTracker names the tracker a result came from. Jackett and Prowlarr tag
// every item with its real source indexer, which matters most on their
// aggregate endpoints, where the configured label would name only the proxy.
func itemTracker(item TorznabItem, label string) string {
	if name := strings.TrimSpace(item.Jackett); name != "" {
		return name
	}
	if name := strings.TrimSpace(item.Prowlarr); name != "" {
		return name
	}
	return label
}

// searchCategories resolves the categories one search sends to one indexer:
// the caller's override when it gave one, the indexer's own setting otherwise.
func searchCategories(config settings.TorznabConfig, override *string) string {
	if override != nil {
		return strings.TrimSpace(*override)
	}
	return config.SearchCategories()
}

// indexerLabel picks a short, human-readable source name for a configured indexer — the
// custom Name if the user set one, otherwise the host's bare domain (searching several
// indexers at once via index=-1 merges results, so the UI needs a way to tell them apart).
func indexerLabel(config settings.TorznabConfig) string {
	if strings.TrimSpace(config.Name) != "" {
		return config.Name
	}
	if u, err := normalizeHost(config.Host); err == nil && u.Host != "" {
		return u.Host
	}
	return config.Host
}

func searchOne(ctx context.Context, host, key, query, label, cat string, offset, limit int) []*models.TorrentDetails {
	u, err := normalizeHost(host)
	if err != nil {
		log.TLogln("Error parsing Torznab host:", err)
		return nil
	}

	q := u.Query()
	q.Set("apikey", key)
	q.Set("t", "search")
	q.Set("q", query)
	if cat != "" {
		q.Set("cat", cat)
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	u.RawQuery = q.Encode()

	req, err := newRequest(ctx, u.String())
	if err != nil {
		log.TLogln("Error creating Torznab request:", err)
		return nil
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		log.TLogln("Error connecting to Torznab:", err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		log.TLogln("Error reading Torznab response:", err)
		return nil
	}

	if resp.StatusCode != http.StatusOK {
		log.TLogln("Torznab returned status:", resp.Status)
		return nil
	}

	if errMsg := parseTorznabError(body); errMsg != "" {
		log.TLogln("Torznab API error:", errMsg)
		return nil
	}

	var torznabResp TorznabResponse
	if err := xml.Unmarshal(body, &torznabResp); err != nil {
		log.TLogln("Error decoding Torznab response:", err)
		return nil
	}

	var results []*models.TorrentDetails
	for _, item := range torznabResp.Channel.Items {
		detail := &models.TorrentDetails{
			Title:      item.Title,
			Name:       item.Title,
			Link:       item.Link,
			CreateDate: parseDate(item.PubDate),
			Tracker:    itemTracker(item, label),
		}

		if len(item.Enclosure) > 0 {
			detail.Link = item.Enclosure[0].URL
			detail.Size = formatSize(item.Enclosure[0].Length)
		} else {
			detail.Size = formatSize(item.Size)
		}

		var leechers, peers int
		var hasLeechers, hasPeers bool
		for _, attr := range item.Attributes {
			switch strings.ToLower(attr.Name) {
			case "magneturl":
				detail.Magnet = attr.Value
				if detail.Hash == "" {
					detail.Hash = extractHash(detail.Magnet)
				}
			case "seeders":
				detail.Seed, _ = strconv.Atoi(attr.Value)
			case "leechers":
				leechers, _ = strconv.Atoi(attr.Value)
				hasLeechers = true
			case "peers":
				peers, _ = strconv.Atoi(attr.Value)
				hasPeers = true
			case "infohash":
				detail.Hash = attr.Value
			}
		}
		if hasLeechers {
			detail.Peer = leechers
		} else if hasPeers {
			detail.Peer = peers
		}

		if detail.Magnet == "" && strings.HasPrefix(detail.Link, "magnet:") {
			detail.Magnet = detail.Link
			if detail.Hash == "" {
				detail.Hash = extractHash(detail.Magnet)
			}
		}

		results = append(results, detail)
	}

	return results
}

func FetchCaps(ctx context.Context, host, key string) (*Caps, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	u, err := normalizeHost(host)
	if err != nil {
		return nil, err
	}

	q := u.Query()
	q.Set("apikey", key)
	q.Set("t", "caps")
	u.RawQuery = q.Encode()

	req, err := newRequest(ctx, u.String())
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status: %s", resp.Status)
	}

	return ParseCapsBytes(body)
}

func ParseCapsBytes(body []byte) (*Caps, error) {
	if errMsg := parseTorznabError(body); errMsg != "" {
		return nil, fmt.Errorf("api error: %s", errMsg)
	}

	var caps Caps
	if err := xml.Unmarshal(body, &caps); err != nil {
		return nil, fmt.Errorf("invalid xml response: %v", err)
	}

	if caps.XMLName.Local != "caps" {
		return nil, fmt.Errorf("unexpected xml root: %s", caps.XMLName.Local)
	}

	return &caps, nil
}

func Test(ctx context.Context, host, key string) error {
	_, err := FetchCaps(ctx, host, key)
	return err
}

func parseTorznabError(body []byte) string {
	var probe torznabError
	if err := xml.Unmarshal(body, &probe); err != nil {
		return ""
	}
	if probe.XMLName.Local != "error" {
		return ""
	}
	msg := probe.Description
	if msg == "" {
		msg = probe.Code
	}
	if msg == "" {
		msg = "unknown error"
	}
	return msg
}

func parseDate(dateStr string) time.Time {
	t, err := time.Parse(time.RFC1123, dateStr)
	if err != nil {
		t, err = time.Parse(time.RFC1123Z, dateStr)
		if err != nil {
			return time.Now()
		}
	}
	return t
}

func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func extractHash(magnet string) string {
	if strings.HasPrefix(magnet, "magnet:?") {
		u, err := url.Parse(magnet)
		if err == nil {
			xt := u.Query().Get("xt")
			if strings.HasPrefix(xt, "urn:btih:") {
				return strings.TrimPrefix(xt, "urn:btih:")
			}
		}
	}
	return ""
}
