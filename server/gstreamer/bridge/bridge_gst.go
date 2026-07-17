//go:build gst

package bridge

import (
	"server/gstreamer"

	"github.com/gin-gonic/gin"
)

func SetupRoute(route gin.IRouter) {
	gstreamer.SetupRoute(route)
}

func BuiltIn() bool {
	return true
}

func Stop() {
	gstreamer.Stop()
}

func Remove(hash string) bool {
	return gstreamer.Remove(hash)
}

// PlaylistHLS reports whether generated M3U playlists should use the HLS
// transcode endpoint for Matroska/WebM entries (the PlaylistHLS setting).
func PlaylistHLS() bool {
	return gstreamer.PlaylistHLS()
}
