// Copied verbatim from feed-engine/internal/worktext/worktext.go (normalizer
// v1). The devserver mirrors /api/v1, and the create path refuses text by
// these rules exactly as the real server does; update from there.

// Package worktext is the one definition of what a work's text is allowed to be:
// how it is normalized, how long it is, when it is blank, and what makes two
// works the same content.
//
// Everything here is a pure function of its input. The body of a work is inside
// its signed content address, so the server never rewrites one: a client signs
// Normalize(body), and the server refuses any body for which IsNormalized is
// false. The web client mirrors these rules in malkuth.js (F33WorkText) and the
// native clients must mirror them too; NormalizerVersion is what they agree on.
//
// Normalizer version 1:
//
//  1. CRLF and lone CR become LF.
//  2. Control characters other than LF and TAB are removed, as are bidi
//     embedding/override/isolate/mark characters and invisible characters
//     (zero-width space, word joiner, BOM, invisible operators, soft hyphen,
//     Hangul fillers, interlinear annotation marks). ZWJ and ZWNJ are kept:
//     they are orthography in Persian and Indic scripts and they bind emoji
//     sequences, so removing them changes what the author wrote.
//  3. A line holding only whitespace becomes empty.
//  4. Runs of blank lines collapse to one blank line.
//  5. Leading and trailing whitespace is trimmed.
//
// Unicode NFC is not part of version 1. The Go standard library has no
// normalization tables; they live in golang.org/x/text, which is not a
// dependency of this module and is not added without approval. The web client
// applies NFC before signing (String.prototype.normalize), so web bodies are
// NFC already. Version 2 adds the NFC check on the server once that dependency
// is approved; until then two canonically-equivalent spellings of one text are
// two texts to the dedup window, which errs toward accepting a post.
package worktext

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// NormalizerVersion names the rule set above. Clients that normalize before
// signing state which version they implement; a change to the rules is a new
// version, never an edit of this one.
const NormalizerVersion = 1

// MaxWeightedLength is the one length limit of a work's body, in weighted units
// (WeightedLength). Latin-script text is one unit a character, so for most
// writers this is the 2000 characters the limit has always been.
const MaxWeightedLength = 2000

// MaxRunes bounds the stored size of a body independently of its weight. A URL
// weighs URLWeight however long it is, so without this a body of long links
// could be arbitrarily large while weighing under the limit.
const MaxRunes = 10000

// URLWeight is what one link weighs, whatever its length, so a long link does
// not eat the author's budget.
const URLWeight = 23

// urlRe is the link pattern the body URL extractor uses (db.workBodyURLRe); the
// weight of a body must count the same links the preview path finds.
var urlRe = regexp.MustCompile(`https?://[^\s"'<>\x00-\x1f]+`)

// urlTrailing is punctuation that ends a sentence rather than a link. It is
// trimmed from a match and weighed as ordinary text.
const urlTrailing = ".,;:!?)'\""

// stripped reports whether a rune is removed by rule 2.
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

// Normalize applies normalizer version NormalizerVersion.
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

// IsNormalized reports whether s is already what Normalize makes of it — the
// condition a signed body must meet, since the server cannot rewrite it.
func IsNormalized(s string) bool {
	return Normalize(s) == s
}

// IsBlank reports whether s has nothing a reader can see: no letter, number,
// punctuation or symbol. Whitespace, marks on nothing, and format characters
// such as ZWJ do not make a body say anything.
func IsBlank(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.L, unicode.N, unicode.P, unicode.S) {
			return false
		}
	}
	return true
}

// runeWeight is what one code point weighs. One unit for the Latin, Greek,
// Cyrillic, Armenian, Hebrew, Arabic, Indic and Georgian blocks and for general
// punctuation, two for everything else (CJK, emoji, …), zero for code points
// that only modify the one before them.
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

// WeightedLength is the length the body limit is measured in. Each link weighs
// URLWeight; everything else weighs what its code points weigh.
func WeightedLength(s string) int {
	n := 0
	last := 0
	for _, m := range urlRe.FindAllStringIndex(s, -1) {
		link := strings.TrimRight(s[m[0]:m[1]], urlTrailing)
		n += textWeight(s[last:m[0]]) + URLWeight
		last = m[0] + len(link)
	}
	return n + textWeight(s[last:])
}

// IsSingleLine reports whether a normalized value fits a one-line field (a poll
// option, a tag).
func IsSingleLine(s string) bool {
	return !strings.ContainsRune(s, '\n')
}

// FingerprintInput is everything that makes a work the same content as another
// by the same author. Ids are work ids, never client CIDs, so two signatures of
// the same thing (different timestamp_ms, same words) fingerprint alike.
type FingerprintInput struct {
	Body         string
	Kind         string
	ParentWorkID string
	QuotedWorkID string
	MediaURLs    []string
	VoiceURL     string
	VideoURL     string
	PollOptions  []string
}

// Fingerprint is the content dedup key. The encoding is JSON of a fixed struct,
// so no two different inputs can share an encoding.
func Fingerprint(in FingerprintInput) (string, error) {
	media := append([]string(nil), in.MediaURLs...)
	sort.Strings(media)
	enc, err := json.Marshal(struct {
		V      int      `json:"v"`
		Body   string   `json:"body"`
		Kind   string   `json:"kind"`
		Parent string   `json:"parent"`
		Quote  string   `json:"quote"`
		Media  []string `json:"media"`
		Voice  string   `json:"voice"`
		Video  string   `json:"video"`
		Poll   []string `json:"poll"`
	}{NormalizerVersion, in.Body, in.Kind, in.ParentWorkID, in.QuotedWorkID, media, in.VoiceURL, in.VideoURL, in.PollOptions})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(enc)
	return hex.EncodeToString(sum[:]), nil
}
