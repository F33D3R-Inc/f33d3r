package jung

import (
	"math"
	"strings"
	"unicode"

	"github.com/f33d3r/feed-engine/internal/model"
)

// MapWork reads a work and places it in the psychological space.
//
// It is a pure function of the work: the same body, kind, media, tags and
// flags always yield the same vector, and nothing about the author or the
// audience enters into it. Evidence is gathered from five layers —
//
//   - the affect lexicon (stems and phrases in the body),
//   - the body's structure (questions, exclamations, capitals, emoji, links,
//     hashtags, pronoun stance, negation, length),
//   - the work's kind and media (a video is not a voice note is not a poll),
//   - its tags,
//   - its adult and gore flags,
//
// — accumulated per axis, squashed so no single layer can saturate a
// coordinate, and normalised (see Vector.Normalise) so the result is unit
// length with every coordinate strictly positive.
//
// This is the one seam through which any work receives a vector. Audio works
// get theirs here too for now; when Zior's audio analysis is wired into this
// brain, its PsychVector for a voice or track replaces the text mapping at
// this seam and nowhere else.
func MapWork(w *model.Work) Vector {
	var acc [Dim]float64
	if w == nil {
		return Neutral()
	}

	body := w.Body
	lower := strings.ToLower(body)

	// ── Layer 1: lexicon ─────────────────────────────────────────────────
	var lex [Dim]float64
	for phrase, entries := range phraseLexicon {
		if n := strings.Count(lower, phrase); n > 0 {
			for _, e := range entries {
				lex[e.axis] += float64(e.weight) * float64(n)
			}
		}
	}
	tokens := tokenise(lower)
	for _, tok := range tokens {
		for _, e := range lookupStem(tok) {
			lex[e.axis] += float64(e.weight)
		}
	}
	// Density, not count: a long post mentioning "love" once is not a love
	// letter. Divide by a sub-linear function of length so short posts keep
	// their intensity and long posts are not punished for having words.
	scale := 1.0 + math.Sqrt(float64(len(tokens)))/4.0
	for i := range lex {
		acc[i] += 1.0 - math.Exp(-lex[i]/scale)
	}

	// ── Layer 2: structure ───────────────────────────────────────────────
	st := structure(body, tokens)
	acc[Tension] += 0.5 * st.questionDensity
	acc[Disruption] += 0.2 * st.questionDensity
	acc[Agency] += 0.4 * st.exclaimDensity
	acc[Release] += 0.4 * st.exclaimDensity
	acc[Agency] += 0.3 * st.capsRatio
	acc[Tension] += 0.2 * st.capsRatio
	acc[Disruption] += 0.2 * st.capsRatio
	acc[Release] += 0.5 * st.emojiJoy
	acc[Disruption] += 0.2 * st.emojiJoy
	acc[Release] += 0.6 * st.emojiHype
	acc[Agency] += 0.3 * st.emojiHype
	acc[Persona] += 0.2 * st.emojiHype
	acc[Attachment] += 0.6 * st.emojiHeart
	acc[Release] += 0.2 * st.emojiHeart
	acc[Shadow] += 0.6 * st.emojiDark
	acc[Attachment] += 0.2 * st.emojiDark
	acc[Tension] += 0.6 * st.emojiAnger
	acc[Shadow] += 0.2 * st.emojiAnger
	acc[Tension] += 0.4 * st.emojiThink
	acc[Integration] += 0.2 * st.emojiThink
	acc[Persona] += 0.3 * st.links
	acc[Agency] += 0.1 * st.links
	acc[Persona] += 0.15 * st.hashtags
	acc[Shadow] += 0.3 * st.firstPerson
	acc[Attachment] += 0.2 * st.firstPerson
	acc[Attachment] += 0.5 * st.firstPlural
	acc[Integration] += 0.2 * st.firstPlural
	acc[Tension] += 0.2 * st.secondPerson
	acc[Persona] += 0.2 * st.secondPerson
	acc[Agency] += 0.1 * st.secondPerson
	acc[Tension] += 0.4 * st.negation
	acc[Shadow] += 0.2 * st.negation
	switch {
	case st.runes >= 280:
		acc[Integration] += 0.2
		acc[Persona] += 0.1
	case st.runes > 0 && st.runes < 40:
		acc[Disruption] += 0.1
		acc[Release] += 0.1
	}

	// ── Layer 3: kind and media ──────────────────────────────────────────
	hasImage := len(w.MediaURLs) > 0 || strings.EqualFold(w.ContentType, "image")
	switch strings.ToLower(w.Kind) {
	case "video", "react_video":
		acc[Agency] += 0.5
		acc[Persona] += 0.5
		hasImage = false
	case "voice":
		acc[Attachment] += 0.5
		acc[Shadow] += 0.3
	case "poll":
		acc[Disruption] += 0.4
		acc[Tension] += 0.4
	case "reply":
		acc[Attachment] += 0.4
	case "quote":
		acc[Tension] += 0.4
	case "thread_post":
		acc[Integration] += 0.3
		acc[Persona] += 0.2
	}
	if w.VideoMasterURL != "" && !strings.EqualFold(w.Kind, "video") && !strings.EqualFold(w.Kind, "react_video") {
		acc[Agency] += 0.3
		acc[Persona] += 0.3
		hasImage = false
	}
	if w.VoiceURL != "" && !strings.EqualFold(w.Kind, "voice") {
		acc[Attachment] += 0.3
		acc[Shadow] += 0.2
	}
	if len(w.PollOptions) > 0 && !strings.EqualFold(w.Kind, "poll") {
		acc[Disruption] += 0.3
		acc[Tension] += 0.3
	}
	if hasImage {
		acc[Persona] += 0.5
	}

	// ── Layer 4: tags ────────────────────────────────────────────────────
	seenTag := make(map[string]bool, len(w.Tags))
	for _, tag := range w.Tags {
		key := strings.ToLower(strings.TrimLeft(strings.TrimSpace(tag), "#"))
		if key == "" || seenTag[key] {
			continue
		}
		seenTag[key] = true
		for _, e := range tagHints[key] {
			acc[e.axis] += float64(e.weight)
		}
	}

	// ── Layer 5: adult flags ─────────────────────────────────────────────
	if w.IsNSFW {
		acc[Shadow] += 0.5
		acc[Disruption] += 0.2
	}
	if w.IsGore {
		acc[Shadow] += 0.8
		acc[Tension] += 0.3
	}

	// Squash and normalise. The squash keeps a coordinate in [0, 1) however
	// much evidence piled onto it, so a wall of "love love love" is very
	// Attachment and not infinitely so.
	var v Vector
	for i := range v {
		v[i] = float32(1.0 - math.Exp(-acc[i]))
	}
	return v.Normalise()
}

// structural features of a body, each already scaled to roughly [0, 1].
type structural struct {
	runes           int
	questionDensity float64
	exclaimDensity  float64
	capsRatio       float64
	emojiJoy        float64
	emojiHype       float64
	emojiHeart      float64
	emojiDark       float64
	emojiAnger      float64
	emojiThink      float64
	links           float64
	hashtags        float64
	firstPerson     float64
	firstPlural     float64
	secondPerson    float64
	negation        float64
}

var (
	firstPersonWords  = set("i", "me", "my", "mine", "myself", "i'm", "im", "i've", "ive", "i'll", "ill", "i'd")
	firstPluralWords  = set("we", "us", "our", "ours", "we're", "were", "we've", "weve", "we'll", "ourselves")
	secondPersonWords = set("you", "your", "yours", "you're", "youre", "you've", "youve", "you'll", "u", "ur", "yourself")
	negationWords     = set("not", "no", "never", "don't", "dont", "can't", "cant", "won't", "wont", "isn't", "isnt",
		"didn't", "didnt", "doesn't", "doesnt", "wasn't", "wasnt", "aren't", "arent", "couldn't", "couldnt",
		"shouldn't", "shouldnt", "wouldn't", "wouldnt", "nothing", "nobody", "nowhere", "neither", "nor")
)

func set(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

func structure(body string, tokens []string) structural {
	var st structural
	if body == "" {
		return st
	}
	var (
		questions, exclaims, sentences int
		upper, letters                 int
		joy, hype, heart, dark, anger  int
		think, links, hashtags         int
	)
	st.runes = 0
	for _, r := range body {
		st.runes++
		switch {
		case r == '?':
			questions++
			sentences++
		case r == '!':
			exclaims++
			sentences++
		case r == '.' || r == '\n':
			sentences++
		case unicode.IsLetter(r):
			letters++
			if unicode.IsUpper(r) {
				upper++
			}
		}
		switch emojiClass(r) {
		case emojiJoy:
			joy++
		case emojiHype:
			hype++
		case emojiHeart:
			heart++
		case emojiDark:
			dark++
		case emojiAnger:
			anger++
		case emojiThink:
			think++
		}
	}
	if sentences == 0 {
		sentences = 1
	}
	st.questionDensity = clamp01(float64(questions) / float64(sentences))
	st.exclaimDensity = clamp01(float64(exclaims) / float64(sentences))
	if letters >= 8 {
		// Below eight letters, capitals are an initialism, not shouting.
		ratio := float64(upper) / float64(letters)
		if ratio > 0.3 {
			st.capsRatio = clamp01((ratio - 0.3) / 0.7)
		}
	}
	st.emojiJoy = saturate(joy)
	st.emojiHype = saturate(hype)
	st.emojiHeart = saturate(heart)
	st.emojiDark = saturate(dark)
	st.emojiAnger = saturate(anger)
	st.emojiThink = saturate(think)

	for _, f := range strings.Fields(body) {
		if strings.HasPrefix(f, "http://") || strings.HasPrefix(f, "https://") {
			links++
		} else if strings.HasPrefix(f, "#") && len(f) > 1 {
			hashtags++
		}
	}
	st.links = saturate(links)
	st.hashtags = clamp01(float64(hashtags) / 3.0)

	if n := len(tokens); n > 0 {
		var fp, fpl, sp, neg int
		for _, t := range tokens {
			switch {
			case firstPersonWords[t]:
				fp++
			case firstPluralWords[t]:
				fpl++
			case secondPersonWords[t]:
				sp++
			}
			if negationWords[t] {
				neg++
			}
		}
		// Pronoun stance as a share of the body, amplified so a few hits in
		// a normal-length post register: five "I"s in fifty words is 0.5.
		st.firstPerson = clamp01(float64(fp) / float64(n) * 5)
		st.firstPlural = clamp01(float64(fpl) / float64(n) * 5)
		st.secondPerson = clamp01(float64(sp) / float64(n) * 5)
		st.negation = clamp01(float64(neg) / float64(n) * 5)
	}
	return st
}

type emojiKind int

const (
	emojiNone emojiKind = iota
	emojiJoy
	emojiHype
	emojiHeart
	emojiDark
	emojiAnger
	emojiThink
)

// emojiClass sorts the emoji that carry a clear affect. Everything else is
// decoration and says nothing.
func emojiClass(r rune) emojiKind {
	switch r {
	case 0x1F602, 0x1F923, 0x1F606, 0x1F601, 0x1F604, 0x1F603, 0x1F600, 0x1F605, 0x1F60A, 0x1F929:
		return emojiJoy // 😂 🤣 😆 😁 😄 😃 😀 😅 😊 🤩
	case 0x1F525, 0x1F389, 0x1F973, 0x1F38A, 0x2728, 0x1F680, 0x1F4AF, 0x1F3C6, 0x1F64C, 0x1F4A5:
		return emojiHype // 🔥 🎉 🥳 🎊 ✨ 🚀 💯 🏆 🙌 💥
	case 0x2764, 0x1F60D, 0x1F970, 0x1F495, 0x1F496, 0x1F90D, 0x1F497, 0x1F49C, 0x1F49B, 0x1F499, 0x1F49A, 0x1F9E1, 0x1F917, 0x1F618:
		return emojiHeart // ❤ 😍 🥰 💕 💖 🤍 💗 💜 💛 💙 💚 🧡 🤗 😘
	case 0x1F622, 0x1F62D, 0x1F480, 0x1F5A4, 0x1F940, 0x1F614, 0x1F494, 0x1F625, 0x1F97A, 0x1F636, 0x1F610:
		return emojiDark // 😢 😭 💀 🖤 🥀 😔 💔 😥 🥺 😶 😐
	case 0x1F621, 0x1F92C, 0x1F47F, 0x1F620, 0x1F644, 0x1F624, 0x1F612:
		return emojiAnger // 😡 🤬 👿 😠 🙄 😤 😒
	case 0x1F914, 0x1F9D0, 0x1F928, 0x1F615, 0x1F62C:
		return emojiThink // 🤔 🧐 🤨 😕 😬
	}
	return emojiNone
}

func saturate(n int) float64 {
	if n <= 0 {
		return 0
	}
	return 1.0 - math.Exp(-float64(n)/2.0)
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// tokenise splits a lowercased body into word tokens. Apostrophes stay
// inside a word so "don't" and "i'm" survive as the tokens the stance and
// negation sets expect; everything else that is not a letter or digit ends a
// token. Hashtag and mention sigils are dropped so "#finally" is "finally".
func tokenise(lower string) []string {
	tokens := make([]string, 0, 32)
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			tokens = append(tokens, b.String())
			b.Reset()
		}
	}
	for _, r := range lower {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' || r == '’':
			if r == '’' {
				r = '\''
			}
			b.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return tokens
}

// lookupStem finds the lexicon entries for a token, trying the token itself
// and then progressively shorter stems produced by stripping common English
// suffixes, so "celebrating" reaches "celebrat" and "shipped" reaches "ship".
func lookupStem(tok string) []lexEntry {
	if e, ok := stemLexicon[tok]; ok {
		return e
	}
	for _, suf := range []string{"ing", "ed", "es", "s", "ly", "er", "est", "ion", "ions", "ness"} {
		if len(tok) > len(suf)+2 && strings.HasSuffix(tok, suf) {
			stem := tok[:len(tok)-len(suf)]
			if e, ok := stemLexicon[stem]; ok {
				return e
			}
			// "shipped" → "shipp" → "ship"; "running" → "runn" → "run".
			if l := len(stem); l > 3 && stem[l-1] == stem[l-2] {
				if e, ok := stemLexicon[stem[:l-1]]; ok {
					return e
				}
			}
		}
	}
	// Prefix match against longer stems for tokens the suffix list did not
	// cover: "vulnerability" begins with "vulnerab".
	for l := len(tok) - 1; l >= 4; l-- {
		if e, ok := stemLexicon[tok[:l]]; ok {
			return e
		}
	}
	return nil
}
