package api

import (
	"encoding/binary"
	"errors"
	"io"
)

// mp4Probe is what the stand-in transcoder learns about an MP4 without
// decoding it: how long it runs and how big its picture is, read off the
// `moov` box the way ffprobe would, so the work carries real numbers rather
// than zeros the player has to guess around.
type mp4Probe struct {
	// Seconds, fractional. Zero when the file has no mvhd.
	DurationSecs float64
	// Display size after the track's rotation matrix is applied — a phone's
	// portrait clip is stored landscape with a 90° matrix, and the player
	// needs the size the viewer will see.
	Width, Height int
}

var errNotMP4 = errors.New("not an ISO base media file")

// probeMP4 walks the top-level boxes for `moov`, then `mvhd` for the
// timescale and duration and each `trak`'s `tkhd` for a picture size. A
// file whose moov sits after a multi-gigabyte mdat is walked by seeking
// past the mdat, not by reading it.
func probeMP4(r io.ReadSeeker) (mp4Probe, error) {
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return mp4Probe{}, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return mp4Probe{}, err
	}
	head := make([]byte, 8)
	if _, err := io.ReadFull(r, head); err != nil || string(head[4:8]) != "ftyp" {
		return mp4Probe{}, errNotMP4
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return mp4Probe{}, err
	}

	var out mp4Probe
	err = walkBoxes(r, 0, size, func(kind string, start, length int64, header int64) error {
		if kind != "moov" {
			return nil
		}
		return walkBoxes(r, start+header, start+length, func(kind string, start, length int64, header int64) error {
			switch kind {
			case "mvhd":
				body, err := readBox(r, start+header, length-header)
				if err != nil {
					return err
				}
				out.DurationSecs = mvhdDuration(body)
			case "trak":
				return walkBoxes(r, start+header, start+length, func(kind string, start, length int64, header int64) error {
					if kind != "tkhd" {
						return nil
					}
					body, err := readBox(r, start+header, length-header)
					if err != nil {
						return err
					}
					if w, h, ok := tkhdSize(body); ok && out.Width == 0 {
						out.Width, out.Height = w, h
					}
					return nil
				})
			}
			return nil
		})
	})
	return out, err
}

// walkBoxes calls visit for each box between from and to, handling the
// 64-bit `largesize` form and the size-zero "to end of file" form.
func walkBoxes(r io.ReadSeeker, from, to int64, visit func(kind string, start, length, header int64) error) error {
	pos := from
	for pos+8 <= to {
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return err
		}
		var hdr [16]byte
		if _, err := io.ReadFull(r, hdr[:8]); err != nil {
			return err
		}
		length := int64(binary.BigEndian.Uint32(hdr[:4]))
		kind := string(hdr[4:8])
		header := int64(8)
		switch length {
		case 0:
			length = to - pos
		case 1:
			if _, err := io.ReadFull(r, hdr[8:16]); err != nil {
				return err
			}
			length = int64(binary.BigEndian.Uint64(hdr[8:16]))
			header = 16
		}
		if length < header || pos+length > to {
			// A box that runs past its parent is a corrupt file; stop rather
			// than read garbage as structure.
			return nil
		}
		if err := visit(kind, pos, length, header); err != nil {
			return err
		}
		pos += length
	}
	return nil
}

func readBox(r io.ReadSeeker, at, length int64) ([]byte, error) {
	if length < 0 || length > 1<<20 {
		return nil, errors.New("mp4: box too large to read")
	}
	if _, err := r.Seek(at, io.SeekStart); err != nil {
		return nil, err
	}
	body := make([]byte, length)
	_, err := io.ReadFull(r, body)
	return body, err
}

// mvhdDuration reads timescale and duration from a Movie Header box body
// (after the 8-byte box header): version 0 keeps 32-bit times, version 1
// 64-bit.
func mvhdDuration(b []byte) float64 {
	if len(b) < 20 {
		return 0
	}
	var timescale, duration float64
	switch b[0] {
	case 1:
		if len(b) < 32 {
			return 0
		}
		timescale = float64(binary.BigEndian.Uint32(b[20:24]))
		duration = float64(binary.BigEndian.Uint64(b[24:32]))
	default:
		timescale = float64(binary.BigEndian.Uint32(b[12:16]))
		duration = float64(binary.BigEndian.Uint32(b[16:20]))
	}
	if timescale <= 0 {
		return 0
	}
	return duration / timescale
}

// tkhdSize reads a Track Header's width and height (16.16 fixed point) and
// applies its rotation matrix. False for a track with no picture — audio.
func tkhdSize(b []byte) (int, int, bool) {
	// version(1) flags(3) then 0: ctime(4) mtime(4) id(4) rsvd(4) dur(4) / 1: ctime(8) mtime(8) id(4) rsvd(4) dur(8)
	off := 4 + 4 + 4 + 4 + 4 + 4
	if b[0] == 1 {
		off = 4 + 8 + 8 + 4 + 4 + 8
	}
	// reserved(8) layer(2) alt(2) volume(2) reserved(2) matrix(36) width(4) height(4)
	need := off + 8 + 2 + 2 + 2 + 2 + 36 + 4 + 4
	if len(b) < need {
		return 0, 0, false
	}
	matrix := b[off+16 : off+16+36]
	w := int(binary.BigEndian.Uint32(b[need-8:need-4]) >> 16)
	h := int(binary.BigEndian.Uint32(b[need-4:need]) >> 16)
	if w == 0 || h == 0 {
		return 0, 0, false
	}
	// A 90° or 270° rotation swaps the axes: a = d = 0 with b and c nonzero.
	a := int32(binary.BigEndian.Uint32(matrix[0:4]))
	bb := int32(binary.BigEndian.Uint32(matrix[4:8]))
	c := int32(binary.BigEndian.Uint32(matrix[12:16]))
	d := int32(binary.BigEndian.Uint32(matrix[16:20]))
	if a == 0 && d == 0 && bb != 0 && c != 0 {
		w, h = h, w
	}
	return w, h, true
}
