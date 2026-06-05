package torrstor

import (
	"os"
	"path/filepath"
	"strconv"
)

// ScanHavePieces inspects `<savePath>/<infoHashHex>/` and returns an
// LSB-first packed bitmap of pieces present locally (size-only heuristic
// matching the "trust file-sizes" resume policy chosen in Etap 2).
//
// A piece is considered "have" if its file exists and:
//   - For non-final pieces: size == pieceLength
//   - For the final piece: size > 0 (we can't tell the exact final-piece
//     length without total_size, so accept any non-empty file)
//
// Returns nil when UseDisk is off, the save path is empty or no cache
// dir exists.
func ScanHavePieces(infoHash [20]byte, numPieces int, pieceLength int64) []byte {
	if !useDisk() || numPieces <= 0 || pieceLength <= 0 {
		return nil
	}
	dir := filepath.Join(savePath(), hashHex(infoHash))
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil
	}
	bitmap := make([]byte, (numPieces+7)/8)
	for i := 0; i < numPieces; i++ {
		path := filepath.Join(dir, strconv.Itoa(i))
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		sz := fi.Size()
		if i == numPieces-1 {
			if sz > 0 {
				bitmap[i/8] |= 1 << uint(i%8)
			}
			continue
		}
		if sz == pieceLength {
			bitmap[i/8] |= 1 << uint(i%8)
		}
	}
	return bitmap
}
