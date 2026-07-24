package jacred

import (
	"context"
	"encoding/json"
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
)

const httpTimeout = 30 * time.Second

var httpClient = &http.Client{Timeout: httpTimeout}

// flexInt tolerates a JSON value that is either a number or a numeric string.
// JacRed serializes torrents straight from its flat-file DB, where a numeric
// field (size, sid, pir, relased, quality) can land as either form.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	s = strings.Trim(s, `"`)
	if s == "" {
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		*f = flexInt(n)
		return nil
	}
	if ff, err := strconv.ParseFloat(s, 64); err == nil {
		*f = flexInt(int64(ff))
	}
	// Unparseable values are ignored (kept at zero) rather than failing the
	// whole response decode.
	return nil
}

// flexStrings tolerates "types" arriving as an array, a single string, or null.
type flexStrings []string

func (f *flexStrings) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if trimmed[0] == '[' {
		var arr []string
		_ = json.Unmarshal(b, &arr)
		*f = arr
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err == nil && s != "" {
			*f = flexStrings{s}
		}
	}
	return nil
}

// item mirrors the JacRed native search response
// (GET /api/v1.0/torrents), which returns a flat JSON array.
type item struct {
	Tracker      string      `json:"tracker"`
	URL          string      `json:"url"`
	Title        string      `json:"title"`
	Size         flexInt     `json:"size"`
	SizeName     string      `json:"sizeName"`
	CreateTime   string      `json:"createTime"`
	SID          flexInt     `json:"sid"`
	PIR          flexInt     `json:"pir"`
	Magnet       string      `json:"magnet"`
	Name         string      `json:"name"`
	OriginalName string      `json:"originalname"`
	Relased      flexInt     `json:"relased"`
	Types        flexStrings `json:"types"`
}

// Search queries the configured JacRed aggregator and maps its results into the
// shared TorrentDetails model. JacRed is a single self-aggregating server (it
// already merges ~20 trackers), so unlike Torznab there is no per-indexer list.
func Search(ctx context.Context, query string) []*models.TorrentDetails {
	if ctx == nil {
		ctx = context.Background()
	}
	set := settings.BTsets()
	if !set.EnableJacRedSearch || strings.TrimSpace(set.JacRedUrl) == "" {
		return nil
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}

	endpoint, err := searchURL(set.JacRedUrl, query, set.JacRedKey)
	if err != nil {
		log.TLogln("Error parsing JacRed URL:", err)
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		log.TLogln("Error creating JacRed request:", err)
		return nil
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		log.TLogln("Error connecting to JacRed:", err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		log.TLogln("Error reading JacRed response:", err)
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		log.TLogln("JacRed returned status:", resp.Status)
		return nil
	}

	return parseItems(body)
}

func parseItems(body []byte) []*models.TorrentDetails {
	var items []item
	if err := json.Unmarshal(body, &items); err != nil {
		log.TLogln("Error decoding JacRed response:", err)
		return nil
	}

	results := make([]*models.TorrentDetails, 0, len(items))
	for _, it := range items {
		magnet := strings.TrimSpace(it.Magnet)
		hash := extractHash(magnet)
		if magnet == "" && hash == "" {
			// Metadata-only rows without a playable source are useless to the
			// client, so drop them.
			continue
		}

		title := firstNonEmpty(it.Title, it.Name, it.OriginalName)
		results = append(results, &models.TorrentDetails{
			Title:      title,
			Name:       firstNonEmpty(it.Name, title),
			Names:      names(it.Name, it.OriginalName),
			Size:       sizeText(it.SizeName, int64(it.Size)),
			CreateDate: parseTime(it.CreateTime),
			Tracker:    it.Tracker,
			Link:       it.URL,
			Year:       int(it.Relased),
			Seed:       int(it.SID),
			Peer:       int(it.PIR),
			Magnet:     magnet,
			Hash:       hash,
		})
	}
	return results
}

// Test validates that a JacRed endpoint is reachable and speaks the native
// search API (a well-formed, possibly empty, JSON array).
func Test(ctx context.Context, host, key string) error {
	if ctx == nil {
		ctx = context.Background()
	}

	endpoint, err := searchURL(host, "test", key)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status: %s", resp.Status)
	}

	var probe []json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return fmt.Errorf("unexpected response (not a JacRed search endpoint)")
	}
	return nil
}

// searchURL builds the native search endpoint from a configured base. The user
// may paste either a bare host (http://127.0.0.1:9117) or a full URL already
// ending in /api/v1.0/torrents; both are normalized to the same request.
func searchURL(base, query, key string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return "", fmt.Errorf("empty host")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}

	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid host: %s", base)
	}

	path := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(strings.ToLower(path), "/api/v1.0/torrents") {
		path += "/api/v1.0/torrents"
	}
	u.Path = path

	q := url.Values{}
	q.Set("search", query)
	if strings.TrimSpace(key) != "" {
		q.Set("apikey", key)
	}
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String(), nil
}

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Now()
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Now()
}

func names(name, original string) []string {
	var out []string
	if n := strings.TrimSpace(name); n != "" {
		out = append(out, n)
	}
	if o := strings.TrimSpace(original); o != "" && !strings.EqualFold(o, strings.TrimSpace(name)) {
		out = append(out, o)
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func sizeText(sizeName string, bytes int64) string {
	if s := strings.TrimSpace(sizeName); s != "" {
		return s
	}
	if bytes <= 0 {
		return ""
	}
	return formatSize(bytes)
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
	if !strings.HasPrefix(magnet, "magnet:?") {
		return ""
	}
	u, err := url.Parse(magnet)
	if err != nil {
		return ""
	}
	for _, xt := range u.Query()["xt"] {
		if strings.HasPrefix(xt, "urn:btih:") {
			return strings.ToLower(strings.TrimPrefix(xt, "urn:btih:"))
		}
	}
	return ""
}
