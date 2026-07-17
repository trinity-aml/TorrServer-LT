//go:build !gst

package bridge

import "github.com/gin-gonic/gin"

func SetupRoute(_ gin.IRouter) {
}

func BuiltIn() bool {
	return false
}

func Stop() {
}

func Remove(_ string) bool {
	return false
}

// PlaylistHLS is always false without the gst build tag: playlists keep their
// direct /stream links because there is nothing to transcode with.
func PlaylistHLS() bool {
	return false
}
