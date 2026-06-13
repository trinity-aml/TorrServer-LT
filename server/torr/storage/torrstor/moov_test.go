package torrstor

import (
	"context"
	"encoding/binary"
	"testing"

	"server/settings"
)

// box builds a top-level MP4 box: 8-byte header (size+type) followed by body.
func box(typ string, body []byte) []byte {
	b := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(b[0:4], uint32(8+len(body)))
	copy(b[4:8], typ)
	copy(b[8:], body)
	return b
}

// box64 builds a box with a 64-bit largesize (size field == 1).
func box64(typ string, body []byte) []byte {
	b := make([]byte, 16+len(body))
	binary.BigEndian.PutUint32(b[0:4], 1)
	copy(b[4:8], typ)
	binary.BigEndian.PutUint64(b[8:16], uint64(16+len(body)))
	copy(b[16:], body)
	return b
}

// readerOf serves bytes from an in-memory file for walkMoov.
func readerOf(data []byte) func(off, n int64) ([]byte, bool) {
	return func(off, n int64) ([]byte, bool) {
		if off < 0 || off >= int64(len(data)) {
			return nil, false
		}
		end := off + n
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		return data[off:end], true
	}
}

func TestWalkMoov(t *testing.T) {
	moovBody := make([]byte, 5000)
	bigMdat := make([]byte, 40000)

	tests := []struct {
		name       string
		file       []byte
		wantOK     bool
		wantStart  int64
		wantLength int64
	}{
		{
			name:       "moov at end (streaming layout)",
			file:       concat(box("ftyp", make([]byte, 24)), box("mdat", bigMdat), box("moov", moovBody)),
			wantOK:     true,
			wantStart:  int64(32 + 40008),
			wantLength: int64(5008),
		},
		{
			name:       "moov at front (faststart)",
			file:       concat(box("ftyp", make([]byte, 24)), box("moov", moovBody), box("mdat", bigMdat)),
			wantOK:     true,
			wantStart:  32,
			wantLength: 5008,
		},
		{
			name:       "free box before mdat then moov",
			file:       concat(box("ftyp", make([]byte, 16)), box("free", make([]byte, 8)), box("mdat", bigMdat), box("moov", moovBody)),
			wantOK:     true,
			wantStart:  int64(24 + 16 + 40008),
			wantLength: 5008,
		},
		{
			name:   "64-bit mdat then moov",
			file:   concat(box("ftyp", make([]byte, 24)), box64("mdat", bigMdat), box("moov", moovBody)),
			wantOK: true,
		},
		{
			name:   "no ftyp — not mp4",
			file:   concat(box("mdat", bigMdat), box("moov", moovBody)),
			wantOK: false,
		},
		{
			name:   "no moov at all",
			file:   concat(box("ftyp", make([]byte, 24)), box("mdat", bigMdat)),
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start, end, ok := walkMoov(readerOf(tc.file), int64(len(tc.file)))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if tc.wantStart != 0 && start != tc.wantStart {
				t.Errorf("start = %d, want %d", start, tc.wantStart)
			}
			if tc.wantLength != 0 && end-start != tc.wantLength {
				t.Errorf("length = %d, want %d", end-start, tc.wantLength)
			}
			// The detected range must hold a real 'moov' box header.
			if string(tc.file[start+4:start+8]) != "moov" {
				t.Errorf("detected range does not start at a moov box")
			}
		})
	}
}

func TestWalkMoovTooLarge(t *testing.T) {
	// A moov claiming > maxAutoMoov must be rejected (fall back to fixed tail).
	huge := box("ftyp", make([]byte, 24))
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr[0:4], 0) // size 0 = extends to EOF
	copy(hdr[4:8], "moov")
	file := concat(huge, hdr, make([]byte, maxAutoMoov+1))
	if _, _, ok := walkMoov(readerOf(file), int64(len(file))); ok {
		t.Fatal("oversized moov should be rejected")
	}
}

// TestLocateMoovFromCache exercises the full path: a multi-piece synthetic MP4
// written into a real Cache, located via piece reads (nil handle since every
// piece is present), with torrent-absolute offset math.
func TestLocateMoovFromCache(t *testing.T) {
	prev := settings.BTsets()
	settings.StoreBTsets(&settings.BTSets{UseDisk: false, CacheSize: 64 * pieceLen})
	t.Cleanup(func() { settings.StoreBTsets(prev) })

	moovBody := make([]byte, 6000)
	file := concat(box("ftyp", make([]byte, 24)), box("mdat", make([]byte, 40000)), box("moov", moovBody))
	fileLen := int64(len(file))
	numPieces := int((fileLen + pieceLen - 1) / pieceLen)

	s := NewStorage()
	h := mkHash(0x4D)
	s.callbackOpen(1, h, numPieces, pieceLen)
	c := s.CacheByHash(h)

	c.mu.Lock()
	for i := 0; i < numPieces; i++ {
		start := int64(i) * pieceLen
		end := start + pieceLen
		if end > fileLen {
			end = fileLen
		}
		p := newPiece(c, i)
		if _, err := p.WriteAt(file[start:end], 0); err != nil {
			c.mu.Unlock()
			t.Fatalf("write piece %d: %v", i, err)
		}
		p.setComplete(true)
		c.pieces[i] = p
	}
	c.mu.Unlock()

	wantStart, wantEnd := int64(32+40008), fileLen
	start, end, ok := c.LocateMoov(context.Background(), nil, 0, fileLen)
	if !ok {
		t.Fatal("moov not located")
	}
	if start != wantStart || end != wantEnd {
		t.Fatalf("moov range = [%d,%d), want [%d,%d)", start, end, wantStart, wantEnd)
	}
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
