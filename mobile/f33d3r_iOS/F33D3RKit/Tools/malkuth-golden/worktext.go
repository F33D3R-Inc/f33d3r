package main

// The text rules of feed-engine/internal/worktext/worktext.go, normalizer
// version 1. Normalize, IsNormalized, IsBlank, runeWeight, textWeight and
// WeightedLength are COPIED VERBATIM; update them from there and regenerate
// after any change to that file.
//
// Every input below is already in Unicode NFC. The clients apply NFC before the
// v1 rules (as malkuth.js does) and the Go standard library has no NFC tables,
// so an input that NFC would change cannot have its answer computed here.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const normalizerVersion = 1
const maxWeightedLength = 2000

const urlWeight = 23

var urlRe = regexp.MustCompile(`https?://[^\s"'<>\x00-\x1f]+`)

const urlTrailing = ".,;:!?)'\""

func stripped(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r < 0x20, r == 0x7F, r >= 0x80 && r <= 0x9F: // C0, DEL, C1
		return true
	case r == 0x061C, r == 0x200E, r == 0x200F, // ALM, LRM, RLM
		r >= 0x202A && r <= 0x202E, // LRE, RLE, PDF, LRO, RLO
		r >= 0x2066 && r <= 0x2069: // LRI, RLI, FSI, PDI
		return true
	case r == 0x00AD, // soft hyphen
		r == 0x180E,                // Mongolian vowel separator
		r == 0x200B,                // zero-width space
		r >= 0x2060 && r <= 0x2064, // word joiner, invisible operators
		r >= 0x206A && r <= 0x206F, // deprecated format characters
		r == 0x3164, r == 0xFFA0,   // Hangul fillers
		r == 0xFEFF,                // BOM / zero-width no-break space
		r >= 0xFFF9 && r <= 0xFFFB: // interlinear annotation
		return true
	}
	return false
}

func Normalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if stripped(r) {
			return -1
		}
		return r
	}, s)

	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		if strings.TrimFunc(line, unicode.IsSpace) == "" {
			if blank {
				continue
			}
			blank = true
			out = append(out, "")
			continue
		}
		blank = false
		out = append(out, line)
	}
	return strings.TrimFunc(strings.Join(out, "\n"), unicode.IsSpace)
}

func IsNormalized(s string) bool {
	return Normalize(s) == s
}

func IsBlank(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.L, unicode.N, unicode.P, unicode.S) {
			return false
		}
	}
	return true
}

func runeWeight(r rune) int {
	switch {
	case unicode.In(r, unicode.Mn, unicode.Me),
		r == 0x200C, r == 0x200D, // ZWNJ, ZWJ
		r >= 0xFE00 && r <= 0xFE0F, // variation selectors
		r >= 0xE0100 && r <= 0xE01EF,
		r >= 0x1F3FB && r <= 0x1F3FF, // emoji skin tone modifiers
		r >= 0xE0020 && r <= 0xE007F: // tag characters (subdivision flags)
		return 0
	case r <= 0x10FF,
		r >= 0x2000 && r <= 0x200D,
		r >= 0x2010 && r <= 0x201F,
		r >= 0x2032 && r <= 0x2037:
		return 1
	}
	return 2
}

func textWeight(s string) int {
	n := 0
	for _, r := range s {
		n += runeWeight(r)
	}
	return n
}

func WeightedLength(s string) int {
	n := 0
	last := 0
	for _, m := range urlRe.FindAllStringIndex(s, -1) {
		link := strings.TrimRight(s[m[0]:m[1]], urlTrailing)
		n += textWeight(s[last:m[0]]) + urlWeight
		last = m[0] + len(link)
	}
	return n + textWeight(s[last:])
}

// ─── Fixture ─────────────────────────────────────────────────────────────────

type textCase struct {
	Name           string `json:"name"`
	Input          string `json:"input"`
	Normalized     string `json:"normalized"`
	IsNormalized   bool   `json:"is_normalized"`
	IsBlank        bool   `json:"is_blank"`
	WeightedLength int    `json:"weighted_length"`
	Runes          int    `json:"runes"`
}

// cp spells a string by its code points, so the invisible characters these
// cases are about are visible in the source.
func cp(points ...rune) string { return string(points) }

func writeWorkText(path string) {
	zwsp, zwj, zwnj, wj, bom := cp(0x200B), cp(0x200D), cp(0x200C), cp(0x2060), cp(0xFEFF)
	inputs := []struct{ name, in string }{
		{"empty", ""},
		{"plain", "hello f33d3r"},
		{"crlf", "one\r\ntwo\rthree\n"},
		{"trim_ascii", "  \t padded \t  "},
		{"trim_unicode_space", cp(0xA0, 0x3000, 0x2003) + "wide" + cp(0x205F, 0xA0)},
		{"blank_lines_collapse", "a\n\n\n\nb\n \n\t\n" + cp(0x3000) + "\nc"},
		{"whitespace_only_line", "a\n   \nb"},
		{"internal_spaces_kept", "a   b\t\tc"},
		{"control_chars", "bell" + cp(0x07) + " nul" + cp(0x00) + " esc" + cp(0x1B) + " del" + cp(0x7F) + " c1" + cp(0x85) + "x"},
		{"bidi_controls", "abc" + cp(0x202E) + "dcba" + cp(0x202C) + " " + cp(0x2066) + "iso" + cp(0x2069) + " " + cp(0x200E) + "mark" + cp(0x200F) + " " + cp(0x061C) + "alm"},
		{"invisibles", "zero" + zwsp + "width soft" + cp(0xAD) + "hyphen word" + wj + "joiner bom" + bom + " fill" + cp(0x3164) + "er"},
		{"zwj_kept", cp(0x1F469) + zwj + cp(0x1F4BB) + " family " + cp(0x1F468) + zwj + cp(0x1F469) + zwj + cp(0x1F467) + " ZWNJ:" + cp(0x0645, 0x06CC) + zwnj + cp(0x062E, 0x0648, 0x0627, 0x0647, 0x0645)},
		{"only_invisible", zwsp + wj + bom},
		{"only_space_and_zwj", " " + zwj + " "},
		{"cjk", "日本語のテキスト"},
		{"hangul", "안녕하세요"},
		{"emoji", "🔥🚀✨"},
		{"emoji_skin_tone", cp(0x1F44D, 0x1F3FD, 0x1F44B, 0x1F3FF)},
		{"emoji_variation", cp(0x2764, 0xFE0F) + " " + cp(0x263A, 0xFE0E)},
		{"flags", cp(0x1F1EF, 0x1F1F5) + " " + cp(0x1F3F4, 0xE0067, 0xE0062, 0xE0073, 0xE0063, 0xE0074, 0xE007F)},
		{"combining_marks", "q" + cp(0x0301) + " x" + cp(0x0308) + " " + cp(0x0915, 0x093C)},
		{"latin_extended", "café naïve Ω Здравствуйте"},
		{"arabic_hindi", "مرحبا नमस्ते"},
		{"general_punctuation", "‘quote’ “double” — dash … ellipsis ′ prime"},
		{"url_https", "see https://f33d3r.com/a/very/long/path/that/goes/on/and/on?x=1&y=2 now"},
		{"url_http", "http://example.com"},
		{"url_trailing_punct", "read https://example.com/page. Then (https://example.com/x)!"},
		{"url_quoted", `"https://example.com/q" and 'http://e.co'`},
		{"url_scheme_only", "https:// and http:/x"},
		{"url_two", "https://a.com https://b.com"},
		{"url_unicode_path", "https://example.com/日本語 tail"},
		{"url_uppercase_scheme", "HTTPS://EXAMPLE.COM"},
		{"newline_in_middle", "line one\nline two"},
		{"poll_option_multiline", "yes\nno"},
		{"punctuation_only", "?!"},
		{"symbols_only", "+ $ €"},
		{"marks_on_nothing", cp(0x0301, 0x0308)},
		{"long_latin", strings.Repeat("a", 2001)},
		{"long_cjk", strings.Repeat("字", 1001)},
	}
	out := make([]textCase, 0, len(inputs))
	for _, c := range inputs {
		out = append(out, textCase{
			Name:           c.name,
			Input:          c.in,
			Normalized:     Normalize(c.in),
			IsNormalized:   IsNormalized(c.in),
			IsBlank:        IsBlank(Normalize(c.in)),
			WeightedLength: WeightedLength(Normalize(c.in)),
			Runes:          utf8.RuneCountInString(Normalize(c.in)),
		})
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]interface{}{
		"generated_by": "feed-engine/internal/worktext/worktext.go, copied verbatim into " +
			"F33D3RKit/Tools/malkuth-golden/worktext.go. is_blank, weighted_length and runes " +
			"are of the normalized text. Every input is already NFC.",
		"normalizer_version":  normalizerVersion,
		"max_weighted_length": maxWeightedLength,
		"cases":               out,
	}); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("wrote %d text cases to %s\n", len(out), path)
}
