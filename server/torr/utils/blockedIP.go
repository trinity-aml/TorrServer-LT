package utils

import (
	"os"
	"path/filepath"

	"server/settings"
)

// ReadBlockedIPText loads the data-dir's `blocklist` file if it exists
// and returns its raw contents. The format is the P2P-style text accepted
// by libtorrent's ip_filter (one rule per line: "Description:from-to" or
// "Description:single-ip"). Returns "" with nil error if the file is
// absent, or "" with an error on I/O failure.
func ReadBlockedIPText() (string, error) {
	path := filepath.Join(settings.Path, "blocklist")
	buf, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(buf), nil
}
