package jacred

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseItemsMapsFields(t *testing.T) {
	body := []byte(`[
		{
			"tracker":"Kinozal",
			"url":"https://kinozal.tv/details.php?id=123456",
			"title":"Дюна / Dune (2021) BDRip 1080p",
			"size":15032385536,
			"sizeName":"14.0 GB",
			"createTime":"2021-11-15 12:30:00",
			"sid":350,
			"pir":12,
			"magnet":"magnet:?xt=urn:btih:ABCDEF0123456789ABCDEF0123456789ABCDEF01&dn=dune",
			"name":"дюна",
			"originalname":"dune",
			"relased":2021,
			"types":["фильм","зарубежный"]
		},
		{
			"tracker":"Rutor",
			"url":"http://rutor.example/1",
			"title":"Numbers As Strings",
			"size":"1048576",
			"sid":"10",
			"pir":"5",
			"relased":"2020",
			"magnet":"magnet:?xt=urn:btih:0000000000000000000000000000000000000000"
		},
		{
			"tracker":"NoSource",
			"title":"Metadata only, no magnet",
			"size":100
		}
	]`)

	res := parseItems(body)
	if len(res) != 2 {
		t.Fatalf("len=%d, want 2 (sourceless row must be dropped)", len(res))
	}

	first := res[0]
	if first.Title != "Дюна / Dune (2021) BDRip 1080p" {
		t.Fatalf("Title=%q", first.Title)
	}
	if first.Size != "14.0 GB" {
		t.Fatalf("Size=%q, want sizeName passthrough", first.Size)
	}
	if first.Seed != 350 || first.Peer != 12 {
		t.Fatalf("Seed/Peer=%d/%d, want 350/12", first.Seed, first.Peer)
	}
	if first.Year != 2021 {
		t.Fatalf("Year=%d, want 2021", first.Year)
	}
	if first.Tracker != "Kinozal" {
		t.Fatalf("Tracker=%q", first.Tracker)
	}
	if first.Hash != "abcdef0123456789abcdef0123456789abcdef01" {
		t.Fatalf("Hash=%q, want lowercased btih", first.Hash)
	}
	if first.Magnet == "" {
		t.Fatal("Magnet empty")
	}
	if len(first.Names) != 2 || first.Names[0] != "дюна" || first.Names[1] != "dune" {
		t.Fatalf("Names=%v, want [дюна dune]", first.Names)
	}

	second := res[1]
	if second.Seed != 10 || second.Peer != 5 || second.Year != 2020 {
		t.Fatalf("string-number parse failed: Seed=%d Peer=%d Year=%d", second.Seed, second.Peer, second.Year)
	}
	if second.Size != "1.0 MiB" {
		t.Fatalf("Size=%q, want formatSize fallback 1.0 MiB", second.Size)
	}
}

func TestSearchURL(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		key     string
		want    string
		wantErr bool
	}{
		{
			name: "bare host no scheme",
			host: "127.0.0.1:9117",
			want: "http://127.0.0.1:9117/api/v1.0/torrents?search=Dune",
		},
		{
			name: "trailing slash with key",
			host: "http://127.0.0.1:9117/",
			key:  "abc",
			want: "http://127.0.0.1:9117/api/v1.0/torrents?apikey=abc&search=Dune",
		},
		{
			name: "full path preserved",
			host: "https://jac.example/api/v1.0/torrents",
			want: "https://jac.example/api/v1.0/torrents?search=Dune",
		},
		{
			name:    "empty",
			host:    "   ",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := searchURL(tt.host, "Dune", tt.key)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractHash(t *testing.T) {
	tests := []struct {
		magnet string
		want   string
	}{
		{"magnet:?xt=urn:btih:ABCDEF0123456789ABCDEF0123456789ABCDEF01&dn=x", "abcdef0123456789abcdef0123456789abcdef01"},
		{"https://tracker/details?id=1", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := extractHash(tt.magnet); got != tt.want {
			t.Fatalf("extractHash(%q)=%q, want %q", tt.magnet, got, tt.want)
		}
	}
}

func TestTestValidatesEndpoint(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer okSrv.Close()

	if err := Test(context.Background(), okSrv.URL, ""); err != nil {
		t.Fatalf("Test on a valid empty-array endpoint failed: %v", err)
	}

	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>not jacred</html>`))
	}))
	defer badSrv.Close()

	if err := Test(context.Background(), badSrv.URL, ""); err == nil {
		t.Fatal("Test on a non-JSON endpoint must fail")
	}

	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer errSrv.Close()

	if err := Test(context.Background(), errSrv.URL, ""); err == nil {
		t.Fatal("Test on a 500 endpoint must fail")
	}
}
