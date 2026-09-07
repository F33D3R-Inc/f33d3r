package api

import (
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
)

// The byte sniffers the upload lane trusts over a declared Content-Type or a
// filename. Same names and same rules as feed-engine's media.go, so the
// handlers that call them lift across unchanged.

// isISOBMFF recognises the MP4 family — MP4, MOV, M4V, M4A — by the `ftyp`
// box every one of them opens with.
func isISOBMFF(head []byte) bool {
	return len(head) >= 8 && string(head[4:8]) == "ftyp"
}

// isEBML recognises the Matroska family — WebM, MKV, and WebM/Opus audio.
func isEBML(head []byte) bool {
	return len(head) >= 4 && head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3
}

// imageExtensionFor names an image by its bytes, falling back to the name
// only for HEIC, which net/http does not sniff. Empty when it is not an image
// this server stores.
func imageExtensionFor(contentType, filename string, data []byte) string {
	sniffed := http.DetectContentType(data)
	for _, ct := range []string{sniffed, contentType} {
		switch ct {
		case "image/jpeg":
			return ".jpg"
		case "image/png":
			return ".png"
		case "image/gif":
			return ".gif"
		case "image/webp":
			return ".webp"
		case "image/heic", "image/heif":
			return ".heic"
		}
	}
	if ext := strings.ToLower(filepath.Ext(filename)); ext == ".heic" || ext == ".heif" {
		return ".heic"
	}
	return ""
}

// audioExtensionFor names a recording by its bytes. Empty when the bytes are
// not an audio container this server stores — so an executable named
// voice.m4a is refused, not kept.
func audioExtensionFor(data []byte) string {
	if len(data) < 4 {
		return ""
	}
	switch {
	case isEBML(data):
		return ".webm"
	case string(data[:4]) == "OggS":
		return ".ogg"
	case string(data[:3]) == "ID3":
		return ".mp3"
	case data[0] == 0xFF && (data[1] == 0xFB || data[1] == 0xF3 || data[1] == 0xF2):
		return ".mp3"
	case data[0] == 0xFF && (data[1] == 0xF1 || data[1] == 0xF9):
		return ".aac"
	case isISOBMFF(data):
		return ".m4a"
	}
	return ""
}

// sniffVideo names the canonical extension for a video container, or false.
func sniffVideo(head []byte) (string, bool) {
	switch {
	case isISOBMFF(head):
		return ".mp4", true
	case isEBML(head):
		return ".webm", true
	}
	return "", false
}

// videoFilenameFor keeps the declared name when its extension agrees with the
// bytes, and otherwise renames to what the bytes say.
func videoFilenameFor(declared string, head []byte) (string, bool) {
	canonical, ok := sniffVideo(head)
	if !ok {
		return "", false
	}
	family := map[string]string{".mp4": ".mp4", ".mov": ".mp4", ".m4v": ".mp4", ".webm": ".webm", ".mkv": ".webm"}
	declaredExt := filepath.Ext(declared)
	if family[strings.ToLower(declaredExt)] == canonical {
		return declared, true
	}
	base := strings.TrimSuffix(declared, declaredExt)
	if base == "" {
		base = "upload"
	}
	return base + canonical, true
}

// readMagic reads the first twelve bytes and rewinds.
func readMagic(file io.ReadSeeker) ([]byte, error) {
	head := make([]byte, 12)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return head[:n], nil
}
