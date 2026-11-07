package torznab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"server/settings"
)

func TestNormalizeHost(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		want    string
		wantErr bool
	}{
		{
			name: "jackett torznab base",
			host: "http://192.168.1.10:9117/api/v2.0/indexers/all/results/torznab/",
			want: "http://192.168.1.10:9117/api/v2.0/indexers/all/results/torznab/api",
		},
		{
			name: "already ends with api",
			host: "http://192.168.1.10:9117/api/v2.0/indexers/all/results/torznab/api",
			want: "http://192.168.1.10:9117/api/v2.0/indexers/all/results/torznab/api",
		},
		{
			name: "already ends with api slash",
			host: "http://192.168.1.10:9117/api/v2.0/indexers/all/results/torznab/api/",
			want: "http://192.168.1.10:9117/api/v2.0/indexers/all/results/torznab/api",
		},
		{
			name: "prowlarr indexer id",
			host: "http://localhost:9696/1",
			want: "http://localhost:9696/1/api",
		},
		{
			name: "scheme default",
			host: "localhost:9117/api/v2.0/indexers/all/results/torznab/",
			want: "http://localhost:9117/api/v2.0/indexers/all/results/torznab/api",
		},
		{
			name:    "empty",
			host:    "   ",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := normalizeHost(tt.host)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", u)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := u.String(); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSearchOneTakesTrackerFromIndexerTags(t *testing.T) {
	const feed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
<channel>
<item><title>A</title><link>magnet:?xt=urn:btih:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa</link><jackettindexer id="rutracker">RuTracker.org</jackettindexer></item>
<item><title>B</title><link>magnet:?xt=urn:btih:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb</link><prowlarrindexer id="7">Kinozal</prowlarrindexer></item>
<item><title>C</title><link>magnet:?xt=urn:btih:cccccccccccccccccccccccccccccccccccccccc</link></item>
</channel>
</rss>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(feed))
	}))
	defer srv.Close()

	got := searchOne(context.Background(), srv.URL, "key", "q", "Jackett", "", 0, 0)
	want := []string{"RuTracker.org", "Kinozal", "Jackett"}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Tracker != w {
			t.Errorf("result %d tracker = %q, want %q", i, got[i].Tracker, w)
		}
	}
}

func TestSearchOneFormatsSizeInBinaryUnits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua == "" || strings.HasPrefix(ua, "Go-http-client") {
			t.Errorf("request without a User-Agent: %q (indexers behind a WAF answer 403)", ua)
		}
		_, _ = w.Write([]byte(`<rss><channel>
<item><title>a</title><enclosure url="magnet:?xt=urn:btih:aa" length="1610612736" type="application/x-bittorrent"/></item>
<item><title>b</title><size>512</size></item>
</channel></rss>`))
	}))
	defer srv.Close()

	results := searchOne(context.Background(), srv.URL, "key", "q", "label", "", 0, 0)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Size != "1.5 GiB" {
		t.Errorf("size = %q, want %q", results[0].Size, "1.5 GiB")
	}
	if results[1].Size != "512 B" {
		t.Errorf("size = %q, want %q", results[1].Size, "512 B")
	}
}

func TestSearchCategoriesResolution(t *testing.T) {
	manual := "1000,8000"
	empty := ""
	tests := []struct {
		name     string
		config   settings.TorznabConfig
		override *string
		want     string
	}{
		{name: "unset config searches movies and tv", want: "5000,2000"},
		{
			name:   "explicit default searches movies and tv",
			config: settings.TorznabConfig{CatType: settings.CategoryDefault},
			want:   "5000,2000",
		},
		{
			name:   "all sends no categories",
			config: settings.TorznabConfig{CatType: settings.CategoryAll, Categories: "1000"},
			want:   "",
		},
		{
			name:   "manual sends the configured list",
			config: settings.TorznabConfig{CatType: settings.CategoryManual, Categories: " 1000,8000 "},
			want:   "1000,8000",
		},
		{
			name:   "manual without a list sends nothing",
			config: settings.TorznabConfig{CatType: settings.CategoryManual},
			want:   "",
		},
		{
			name:     "request override wins over the config",
			config:   settings.TorznabConfig{CatType: settings.CategoryManual, Categories: "1000"},
			override: &manual,
			want:     "1000,8000",
		},
		{
			name:     "empty override searches all categories",
			config:   settings.TorznabConfig{CatType: settings.CategoryDefault},
			override: &empty,
			want:     "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := searchCategories(tc.config, tc.override); got != tc.want {
				t.Errorf("searchCategories() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSearchOneSendsTheResolvedCategories(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()["cat"]
		_, _ = w.Write([]byte(`<rss><channel></channel></rss>`))
	}))
	defer srv.Close()

	searchOne(context.Background(), srv.URL, "key", "q", "label", "5000,2000", 0, 0)
	if len(got) != 1 || got[0] != "5000,2000" {
		t.Errorf("cat sent = %v, want [5000,2000]", got)
	}
	searchOne(context.Background(), srv.URL, "key", "q", "label", "", 0, 0)
	if len(got) != 0 {
		t.Errorf("cat sent = %v, want none", got)
	}
}
