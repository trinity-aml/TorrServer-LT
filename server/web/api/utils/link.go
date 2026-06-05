// Package utils is a thin alias layer for callers that historically
// reached into web/api/utils for ParseLink-style helpers. The actual
// parsing now lives in server/torr, on top of the libtorrent shim.
//
// Kept so web handlers, settings migration and tgbot don't need to be
// aware of the package boundary moving.
package utils

import (
	"mime/multipart"

	"server/torr"
	"server/torrshash"
)

// ParseLink dispatches a string (magnet/url/file/hash/torrs token) to
// the appropriate parser.
func ParseLink(link string) (*torr.TorrentSpec, error) {
	return torr.ParseLink(link)
}

// ParseFile reads a multipart upload and returns the spec.
func ParseFile(file multipart.File) (*torr.TorrentSpec, error) {
	return torr.ParseReader(file)
}

// ParseFromBytes parses an in-memory .torrent payload.
func ParseFromBytes(data []byte) (*torr.TorrentSpec, error) {
	return torr.ParseBytes(data)
}

// ParseTorrsHash decodes a torrs:// (or bare base62) token.
func ParseTorrsHash(token string) (*torr.TorrentSpec, *torrshash.TorrsHash, error) {
	return torr.ParseTorrsHash(token)
}
