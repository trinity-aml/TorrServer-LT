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

// ProxyMode reports whether GStreamer proxy mode is on: generated M3U
// playlists route supported (Matroska/WebM) entries through the HLS transcode
// endpoint (the Proxy setting).
func ProxyMode() bool {
	return gstreamer.ProxyMode()
}
