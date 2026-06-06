package rutor

import (
	"compress/flate"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"server/rutor/models"
)

// TestParseChannel / TestParseArr parse the entire production rutor.ls
// (~390k torrents, 250+ MB decompressed) and are manual stress/inspection
// tools, not routine units — they don't finish in a normal `go test` window.
// Skipped unless RUTOR_FULL_PARSE is set.
func skipUnlessFullParse(t *testing.T) {
	t.Helper()
	if os.Getenv("RUTOR_FULL_PARSE") == "" {
		t.Skip("set RUTOR_FULL_PARSE=1 to parse the full rutor.ls (slow)")
	}
}

func TestParseChannel(t *testing.T) {
	skipUnlessFullParse(t)
	channel := make(chan *models.TorrentDetails, 0)
	var ftors []*models.TorrentDetails
	go func() {
		for torr := range channel {
			ftors = append(ftors, torr)
		}
	}()

	path, _ := os.Getwd()
	ff, err := os.Open(filepath.Join(path, "rutor.ls"))
	if err == nil {
		defer ff.Close()
		r := flate.NewReader(ff)
		defer r.Close()
		dec := json.NewDecoder(r)

		_, err := dec.Token()
		if err != nil {
			t.Error(err)
		}

		for dec.More() {
			var torr *models.TorrentDetails
			err = dec.Decode(&torr)
			if err != nil {
				t.Error(err)
			}
			channel <- torr
		}
		close(channel)
	} else {
		t.Error(err)
	}
}

func TestParseArr(t *testing.T) {
	skipUnlessFullParse(t)
	var ftors []*models.TorrentDetails
	path, _ := os.Getwd()
	ff, err := os.Open(filepath.Join(path, "rutor.ls"))
	if err == nil {
		defer ff.Close()
		r := flate.NewReader(ff)
		defer r.Close()
		dec := json.NewDecoder(r)

		_, err := dec.Token()
		if err != nil {
			t.Error(err)
		}

		for dec.More() {
			var torr *models.TorrentDetails
			err = dec.Decode(&torr)
			if err != nil {
				t.Error(err)
			}
			ftors = append(ftors, torr)
			fmt.Println(len(ftors))
		}
	} else {
		t.Error(err)
	}
}
