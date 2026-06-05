// Package lt is the Go-side facade over the libtorrent C-shim.
//
// This is the PoC entry point — only exposes the libtorrent version string.
// Real session / torrent / storage bindings land in later milestones.
package lt

/*
#cgo CXXFLAGS: -std=c++17
#cgo pkg-config: libtorrent-rasterbar
#include "lt_shim.h"
*/
import "C"

// Version returns the libtorrent version string the shim was compiled against.
func Version() string {
	return C.GoString(C.lt_shim_version())
}
