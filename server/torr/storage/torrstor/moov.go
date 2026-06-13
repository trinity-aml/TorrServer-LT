package torrstor

import (
	"context"
	"encoding/binary"
	"io"

	"server/lt"
)

// maxAutoMoov bounds an auto-detected moov: a structurally-valid parse can in
// principle point at an enormous range (a misparse, or a pathological file);
// beyond this the caller falls back to the fixed tail buffer rather than pin
// tens of MB of RAM. 64 MB covers even long 4K moov atoms.
const maxAutoMoov = 64 << 20

// LocateMoov walks the top-level MP4 boxes of the file at
// [fileOffset, fileOffset+fileLength) within the torrent and returns the
// torrent-absolute byte range [start, end) of its `moov` atom — the index a
// player must read in full before it can start or seek. ok is false when the
// file isn't an MP4 (no leading `ftyp` box), `moov` isn't found, the range is
// implausibly large, or fetching the header pieces fails / times out (ctx);
// callers then fall back to a fixed tail buffer.
//
// Only the pieces holding the box headers are fetched on demand — typically the
// first piece (carrying `ftyp` + the `mdat` header) and the piece where `moov`
// begins near the end — never the whole file. Fetched header pieces are raised
// to top priority so they arrive quickly.
func (c *Cache) LocateMoov(ctx context.Context, lh *lt.Torrent, fileOffset, fileLength int64) (start, end int64, ok bool) {
	plen := c.PieceLength
	if plen <= 0 || fileLength < 16 {
		return 0, 0, false
	}

	// read returns n bytes at file-local offset off, pulling (and waiting on)
	// whatever pieces cover them. Returns ok=false on any miss so the walk
	// aborts and the caller falls back.
	read := func(off, n int64) ([]byte, bool) {
		if off < 0 || off >= fileLength {
			return nil, false
		}
		if off+n > fileLength {
			n = fileLength - off
		}
		out := make([]byte, 0, n)
		for int64(len(out)) < n {
			abs := fileOffset + off + int64(len(out))
			piece := int(abs / plen)
			pieceOff := abs - int64(piece)*plen
			if !c.Have(piece) {
				if lh == nil {
					return nil, false
				}
				if lh.HasPiece(piece) {
					_ = lh.WeDontHave(piece, 7) // evicted but libtorrent has it: re-fetch
				}
				_ = lh.SetPiecePriority(piece, 7)
				_ = lh.SetPieceDeadline(piece, 0, false)
				if !c.WaitForPiece(ctx, piece) {
					return nil, false
				}
			}
			want := n - int64(len(out))
			if avail := plen - pieceOff; want > avail {
				want = avail
			}
			buf := make([]byte, want)
			m, err := c.readPiece(piece, pieceOff, buf)
			if m > 0 {
				out = append(out, buf[:m]...)
			}
			if m == 0 || (err != nil && err != io.EOF) {
				return nil, false
			}
		}
		return out, true
	}

	ms, me, ok := walkMoov(read, fileLength)
	if !ok {
		return 0, 0, false
	}
	return fileOffset + ms, fileOffset + me, true
}

// walkMoov is the pure MP4 box walk behind LocateMoov: it reads box headers via
// `read` (file-local offsets) and returns the `moov` box's [start, end) in
// file-local coordinates. Split out so the parser is unit-testable without a
// libtorrent handle or a populated cache.
func walkMoov(read func(off, n int64) ([]byte, bool), fileLength int64) (start, end int64, ok bool) {
	var off int64
	for i := 0; i < 64 && off+8 <= fileLength; i++ {
		h, hok := read(off, 16)
		if !hok || len(h) < 8 {
			return 0, 0, false
		}
		size := int64(binary.BigEndian.Uint32(h[0:4]))
		typ := string(h[4:8])
		if i == 0 && typ != "ftyp" {
			return 0, 0, false // not an MP4 — let the caller use the fixed buffer
		}
		hlen := int64(8)
		switch size {
		case 1: // 64-bit largesize follows the type
			if len(h) < 16 {
				return 0, 0, false
			}
			size = int64(binary.BigEndian.Uint64(h[8:16]))
			hlen = 16
		case 0: // extends to EOF
			size = fileLength - off
		}
		if typ == "moov" {
			e := off + size
			if e > fileLength {
				e = fileLength
			}
			if e-off > maxAutoMoov {
				return 0, 0, false
			}
			return off, e, true
		}
		if size < hlen {
			return 0, 0, false // malformed box
		}
		off += size
	}
	return 0, 0, false
}
