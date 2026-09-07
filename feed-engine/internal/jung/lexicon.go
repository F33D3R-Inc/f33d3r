package jung

// The affect lexicon: word stems and short phrases that carry evidence for an
// axis. This is the vocabulary MapWork reads a body against. Every entry is
// a stem — "celebrat" matches celebrate, celebrated, celebrating,
// celebration — and matching is done on lowercased tokens after a light
// suffix strip, so the list is written in root forms.
//
// The lists are deliberately affective rather than topical. A word belongs
// here because of the psychological posture it expresses, not the subject it
// names: "ship" is Agency because it is the act of releasing work into the
// world, whatever the work is. Topical hints (sports, art, mental health)
// come from tags, in tagHints below, and carry less weight.
//
// A stem may appear under more than one axis when the word genuinely carries
// both; "raw" is Shadow and a little Disruption, "drop" is Release and a
// little Agency. The weight is the evidence a single occurrence contributes.

type lexEntry struct {
	axis   Axis
	weight float32
}

// stemLexicon maps a stem to the axes it is evidence for.
var stemLexicon = map[string][]lexEntry{}

// phraseLexicon holds multi-word expressions matched against the whole
// lowercased body before tokenising, because "change my mind" is one signal
// and three unrelated words otherwise.
var phraseLexicon = map[string][]lexEntry{
	"hot take":          {{Disruption, 1.0}, {Tension, 0.6}},
	"change my mind":    {{Disruption, 1.0}, {Tension, 0.6}},
	"unpopular opinion": {{Disruption, 0.9}, {Tension, 0.6}},
	"real talk":         {{Shadow, 0.8}, {Integration, 0.3}},
	"to be honest":      {{Shadow, 0.7}},
	"not gonna lie":     {{Shadow, 0.7}},
	"i miss":            {{Attachment, 1.0}, {Shadow, 0.3}},
	"thank you":         {{Attachment, 0.9}, {Integration, 0.4}},
	"thanks to":         {{Attachment, 0.7}},
	"we did it":         {{Release, 1.0}, {Attachment, 0.5}},
	"let's go":          {{Agency, 1.0}, {Release, 0.5}},
	"lets go":           {{Agency, 1.0}, {Release, 0.5}},
	"just shipped":      {{Agency, 1.0}, {Persona, 0.6}, {Release, 0.5}},
	"just launched":     {{Agency, 1.0}, {Persona, 0.8}, {Release, 0.5}},
	"now live":          {{Persona, 0.8}, {Release, 0.6}},
	"out now":           {{Persona, 0.8}, {Release, 0.7}},
	"coming soon":       {{Persona, 0.7}, {Tension, 0.3}},
	"can't wait":        {{Tension, 0.6}, {Agency, 0.4}},
	"cant wait":         {{Tension, 0.6}, {Agency, 0.4}},
	"i can't":           {{Shadow, 0.6}, {Tension, 0.5}},
	"i cant":            {{Shadow, 0.6}, {Tension, 0.5}},
	"no one":            {{Shadow, 0.6}},
	"nobody":            {{Shadow, 0.6}},
	"on my own":         {{Shadow, 0.5}, {Agency, 0.4}},
	"lesson learned":    {{Integration, 1.0}},
	"looking back":      {{Integration, 0.9}},
	"at peace":          {{Integration, 1.0}},
	"made peace":        {{Integration, 1.0}},
	"figured out":       {{Integration, 0.8}, {Release, 0.3}},
	"turns out":         {{Integration, 0.5}, {Disruption, 0.3}},
	"wait what":         {{Disruption, 0.9}},
	"what the":          {{Disruption, 0.7}, {Tension, 0.4}},
	"plot twist":        {{Disruption, 1.0}},
	"i love":            {{Attachment, 1.0}},
	"love you":          {{Attachment, 1.0}},
	"my heart":          {{Attachment, 0.8}, {Shadow, 0.2}},
	"so proud":          {{Persona, 0.8}, {Attachment, 0.5}, {Release, 0.4}},
	"i'm done":          {{Release, 0.8}, {Tension, 0.4}},
	"im done":           {{Release, 0.8}, {Tension, 0.4}},
	"it's over":         {{Release, 0.7}, {Shadow, 0.3}},
	"its over":          {{Release, 0.7}, {Shadow, 0.3}},
	"finally":           {{Release, 1.0}},
	"why is":            {{Tension, 0.6}},
	"why do":            {{Tension, 0.6}},
	"why does":          {{Tension, 0.6}},
	"how is this":       {{Tension, 0.7}},
	"not okay":          {{Tension, 0.7}, {Shadow, 0.4}},
	"not ok":            {{Tension, 0.7}, {Shadow, 0.4}},
	"stay tuned":        {{Persona, 0.7}, {Tension, 0.3}},
	"link in bio":       {{Persona, 1.0}},
	"new video":         {{Persona, 0.8}, {Agency, 0.3}},
	"new track":         {{Persona, 0.8}, {Agency, 0.3}},
	"new post":          {{Persona, 0.6}},
	"day one":           {{Agency, 0.8}},
	"day 1":             {{Agency, 0.8}},
	"grind":             {{Agency, 0.9}},
}

func init() {
	// Register the stem lists. Building the map from per-axis lists keeps
	// each axis readable as a vocabulary in its own right.
	add := func(axis Axis, weight float32, stems ...string) {
		for _, s := range stems {
			stemLexicon[s] = append(stemLexicon[s], lexEntry{axis, weight})
		}
	}

	// Persona — showing the work, the brand, the polish, the announcement.
	add(Persona, 1.0,
		"craft", "launch", "proud", "polish", "announc", "brand", "portfolio",
		"showcas", "reveal", "premier", "present", "unveil", "featur", "spotlight",
		"studio", "design", "produc", "render", "mix", "master", "edit", "shoot",
		"collab", "sponsor", "partner", "client", "commission", "release",
		"official", "exclusive", "preview", "teaser", "trailer", "cover", "aesthetic",
		"outfit", "look", "style", "fit", "vibe", "curat", "collection", "print",
		"merch", "shop", "store", "sale", "preorder", "pre-order", "limited",
	)
	add(Persona, 0.6,
		"new", "update", "version", "v2", "beta", "demo", "profile", "bio",
		"follow", "subscribe", "share", "check", "link", "thread", "recap",
	)

	// Shadow — the dark, the hidden, the true, the alone.
	add(Shadow, 1.0,
		"dark", "truth", "hide", "hidden", "pain", "alone", "fear", "raw", "hurt",
		"grief", "griev", "loss", "lost", "lonely", "lonel", "shame", "guilt",
		"secret", "regret", "trauma", "wound", "scar", "numb", "empty", "void",
		"depress", "anxi", "panic", "cry", "tear", "broken", "shatter", "haunt",
		"nightmare", "demon", "ghost", "death", "die", "dead", "suicid", "abuse",
		"addict", "relapse", "sober", "struggl", "suffer", "silence", "silent",
		"confess", "honest", "vulnerab", "expos", "ugly", "monster", "sin",
		"despair", "hopeless", "worthless", "nothing", "tired", "exhaust", "burnout",
	)
	add(Shadow, 0.6,
		"night", "midnight", "3am", "sad", "miss", "sorry", "apolog", "mistake",
		"fail", "quit", "gave up", "cold", "grey", "gray", "black", "rain", "storm",
		"real", "actually", "underneath", "beneath", "inside", "deep",
	)

	// Agency — doing, driving, building, winning, now.
	add(Agency, 1.0,
		"build", "built", "ship", "shipp", "go", "run", "fight", "push", "now",
		"win", "won", "hustl", "grind", "train", "lift", "sprint", "launch",
		"execut", "deliver", "crush", "kill", "dominat", "conquer", "attack",
		"charge", "drive", "driven", "move", "momentum", "power", "strong", "strength",
		"beast", "warrior", "champion", "compet", "race", "goal", "target", "deadline",
		"focus", "discipline", "commit", "start", "begin", "lets", "let's", "action",
		"work", "working", "hard", "harder", "faster", "stronger", "level", "grow",
		"scale", "growth", "achiev", "accomplish", "succeed", "success", "progress",
	)
	add(Agency, 0.6,
		"today", "tonight", "tomorrow", "week", "daily", "streak", "day", "plan",
		"step", "next", "keep", "going", "again", "more", "every", "never stop",
		"do", "make", "made", "create", "coding", "code", "deploy", "fix",
	)

	// Integration — calm, whole, balanced, learned, resolved.
	add(Integration, 1.0,
		"calm", "whole", "balanc", "learn", "learned", "peace", "peaceful", "resolv",
		"heal", "healing", "accept", "forgiv", "grateful", "gratitude", "reflect",
		"perspective", "wisdom", "wise", "lesson", "understand", "understood", "clarity",
		"clear", "ground", "center", "centre", "still", "quiet", "breath", "breathe",
		"meditat", "mindful", "present", "patien", "slow", "gentle", "rest", "restor",
		"harmony", "align", "integrat", "mature", "growth", "journey", "process",
		"closure", "settle", "steady", "stable", "secure", "trust", "faith", "hope",
		"meaning", "purpose", "enough", "okay", "fine", "content", "satisf",
	)
	add(Integration, 0.6,
		"think", "thought", "realis", "realiz", "notice", "remember", "years", "ago",
		"finally understand", "long", "history", "story", "chapter", "season",
		"morning", "sunrise", "garden", "nature", "walk", "read", "book", "philosoph",
	)

	// Attachment — love, missing, together, family, heart, thanks, friends.
	add(Attachment, 1.0,
		"love", "miss", "together", "family", "heart", "thank", "friend", "hug",
		"kiss", "cuddle", "partner", "wife", "husband", "boyfriend", "girlfriend",
		"mom", "mum", "dad", "mother", "father", "sister", "brother", "daughter",
		"son", "baby", "kid", "child", "children", "grandma", "grandpa", "nana",
		"home", "belong", "care", "caring", "kind", "kindness", "warm", "tender",
		"soft", "sweet", "dear", "darling", "beloved", "crush", "date", "wedding",
		"married", "marry", "anniversary", "birthday", "cherish", "adore", "appreciat",
		"support", "community", "team", "crew", "squad", "fam", "bestie", "bff",
		"us", "we", "our", "ours", "each other", "reunion", "visit", "call", "text",
	)
	add(Attachment, 0.6,
		"people", "everyone", "someone", "person", "you all", "yall", "y'all", "guys",
		"welcome", "safe", "comfort", "cozy", "cosy", "coffee", "dinner", "cook",
		"dog", "cat", "puppy", "kitten", "pet",
	)

	// Disruption — chaos, breaking, the random, the weird, the trickster.
	add(Disruption, 1.0,
		"chaos", "chaotic", "break", "wtf", "lol", "lmao", "lmfao", "random", "weird",
		"wild", "insane", "crazy", "unhinged", "cursed", "bizarre", "absurd", "meme",
		"shitpost", "troll", "prank", "joke", "funny", "hilarious", "ridiculous",
		"nonsense", "mess", "messy", "glitch", "bug", "broke", "hack", "hacked",
		"rogue", "rebel", "riot", "anarchy", "disrupt", "shake", "flip", "twist",
		"surprise", "unexpected", "sudden", "plot", "what", "huh", "wait", "hold on",
		"experiment", "mutant", "hybrid", "remix", "mashup", "genre", "bend",
		"strange", "odd", "quirky", "goofy", "silly", "dumb", "stupid", "why not",
	)
	add(Disruption, 0.6,
		"idk", "tbh", "ngl", "bro", "bruh", "dude", "yo", "lowkey", "highkey",
		"literally", "vibes", "energy", "mood", "different", "new idea", "pivot",
		"try", "test", "maybe", "probably", "somehow", "accident", "oops",
	)

	// Tension — but, why, versus, anger, wrong, broken, late, waiting.
	add(Tension, 1.0,
		"but", "why", "vs", "versus", "angry", "anger", "wrong", "broken", "late",
		"wait", "waiting", "against", "argue", "argument", "debate", "fight", "conflict",
		"disagree", "problem", "issue", "stuck", "block", "blocked", "delay", "still",
		"yet", "unfair", "hate", "furious", "rage", "annoy", "frustrat", "irritat",
		"stress", "pressure", "tense", "tension", "urgent", "crisis", "emergency",
		"warning", "danger", "threat", "risk", "worry", "worried", "nervous", "scared",
		"uncertain", "doubt", "question", "confus", "dilemma", "torn", "conflicted",
		"should", "must", "need", "have to", "deadline", "overdue", "behind", "unresolved",
		"complain", "rant", "controvers", "drama", "toxic", "lie", "liar", "fake", "scam",
		"ban", "banned", "cancel", "boycott", "protest", "politic", "election", "vote",
	)
	add(Tension, 0.6,
		"however", "although", "though", "except", "unless", "instead", "actually",
		"seriously", "honestly", "really", "hmm", "ugh", "smh", "nope", "no", "not",
		"never", "nothing", "nobody", "cant", "can't", "wont", "won't", "dont", "don't",
		"isnt", "isn't", "didnt", "didn't", "shouldnt", "shouldn't", "if", "unless",
	)

	// Release — finally, done, free, celebration, the drop, the payoff.
	add(Release, 1.0,
		"finally", "done", "free", "freedom", "celebrat", "drop", "dropped", "party",
		"cheers", "congrat", "congrats", "yay", "yes", "yesss", "woo", "woohoo", "hype",
		"relief", "reliev", "exhale", "breathe out", "let go", "letting go", "release",
		"released", "out", "live", "victory", "winning", "won", "made it", "nailed",
		"crushed it", "killed it", "smashed", "banger", "fire", "lit", "goat",
		"epic", "legend", "legendary", "amazing", "incredible", "awesome", "best",
		"happy", "happiest", "joy", "joyful", "bliss", "ecstatic", "thrilled", "excited",
		"stoked", "pumped", "vacation", "holiday", "weekend", "friday", "summer",
		"beach", "sun", "sunshine", "dance", "dancing", "sing", "laugh", "laughing",
		"smile", "smiling", "cheer", "toast", "champagne", "graduat", "milestone",
	)
	add(Release, 0.6,
		"good", "great", "nice", "cool", "sweet", "perfect", "beautiful", "gorgeous",
		"wow", "omg", "yeah", "yep", "haha", "hehe", "lets go", "let's go",
		"finish", "finished", "complete", "completed", "over", "end", "ended",
	)
}

// tagHints maps lowercase tags to axis evidence. Tags are topical, and a
// topic is weaker evidence of posture than a word choice, so hint weights sit
// below the lexicon's.
var tagHints = map[string][]lexEntry{
	// Persona: craft on display.
	"art": {{Persona, 0.6}}, "design": {{Persona, 0.6}}, "photography": {{Persona, 0.6}},
	"fashion": {{Persona, 0.6}}, "style": {{Persona, 0.5}}, "ootd": {{Persona, 0.6}},
	"music": {{Persona, 0.4}, {Attachment, 0.2}}, "producer": {{Persona, 0.5}, {Agency, 0.3}},
	"illustration": {{Persona, 0.6}}, "digitalart": {{Persona, 0.6}}, "3d": {{Persona, 0.5}},
	"animation": {{Persona, 0.5}}, "portfolio": {{Persona, 0.7}}, "brand": {{Persona, 0.7}},
	"launch": {{Persona, 0.6}, {Release, 0.4}}, "showcase": {{Persona, 0.7}},
	// Shadow: the hard interior.
	"mentalhealth": {{Shadow, 0.7}, {Integration, 0.3}}, "depression": {{Shadow, 0.8}},
	"anxiety": {{Shadow, 0.7}, {Tension, 0.3}}, "trauma": {{Shadow, 0.8}}, "grief": {{Shadow, 0.8}},
	"vent": {{Shadow, 0.7}, {Tension, 0.3}}, "confession": {{Shadow, 0.8}}, "dark": {{Shadow, 0.7}},
	"horror": {{Shadow, 0.7}, {Tension, 0.3}}, "goth": {{Shadow, 0.6}, {Persona, 0.3}},
	"nsfw": {{Shadow, 0.6}}, "adult": {{Shadow, 0.6}}, "sober": {{Shadow, 0.5}, {Integration, 0.5}},
	// Agency: drive.
	"fitness": {{Agency, 0.7}}, "gym": {{Agency, 0.7}}, "hustle": {{Agency, 0.8}},
	"startup": {{Agency, 0.6}, {Persona, 0.3}}, "buildinpublic": {{Agency, 0.7}, {Persona, 0.5}},
	"buildingpublic": {{Agency, 0.7}, {Persona, 0.5}}, "coding": {{Agency, 0.5}},
	"programming": {{Agency, 0.5}}, "dev": {{Agency, 0.5}}, "sports": {{Agency, 0.6}, {Tension, 0.2}},
	"nfl": {{Agency, 0.6}, {Tension, 0.3}}, "nba": {{Agency, 0.6}, {Tension, 0.3}},
	"soccer": {{Agency, 0.6}, {Tension, 0.3}}, "football": {{Agency, 0.6}, {Tension, 0.3}},
	"ufc": {{Agency, 0.7}, {Tension, 0.4}}, "mma": {{Agency, 0.7}, {Tension, 0.4}},
	"boxing": {{Agency, 0.7}, {Tension, 0.4}}, "running": {{Agency, 0.6}}, "marathon": {{Agency, 0.7}},
	"productivity": {{Agency, 0.6}, {Integration, 0.2}}, "entrepreneur": {{Agency, 0.7}, {Persona, 0.3}},
	"gamedev": {{Agency, 0.5}, {Persona, 0.4}}, "esports": {{Agency, 0.6}, {Tension, 0.3}},
	// Integration: the settled mind.
	"mindfulness": {{Integration, 0.8}}, "meditation": {{Integration, 0.8}},
	"yoga": {{Integration, 0.7}}, "wellness": {{Integration, 0.6}}, "philosophy": {{Integration, 0.7}},
	"selfcare": {{Integration, 0.6}, {Attachment, 0.2}}, "healing": {{Integration, 0.7}, {Shadow, 0.3}},
	"gratitude": {{Integration, 0.6}, {Attachment, 0.4}}, "books": {{Integration, 0.5}},
	"reading": {{Integration, 0.5}}, "nature": {{Integration, 0.5}}, "garden": {{Integration, 0.5}},
	"stoic": {{Integration, 0.7}}, "stoicism": {{Integration, 0.7}}, "slowliving": {{Integration, 0.7}},
	"spirituality": {{Integration, 0.6}, {Shadow, 0.2}}, "faith": {{Integration, 0.6}, {Attachment, 0.2}},
	// Attachment: closeness.
	"family": {{Attachment, 0.8}}, "love": {{Attachment, 0.8}}, "friends": {{Attachment, 0.7}},
	"relationship": {{Attachment, 0.7}}, "wedding": {{Attachment, 0.7}, {Release, 0.3}},
	"baby": {{Attachment, 0.8}}, "parenting": {{Attachment, 0.7}}, "momlife": {{Attachment, 0.7}},
	"dadlife": {{Attachment, 0.7}}, "couple": {{Attachment, 0.7}}, "dog": {{Attachment, 0.5}},
	"cat": {{Attachment, 0.5}}, "pets": {{Attachment, 0.5}}, "community": {{Attachment, 0.6}},
	"cozy": {{Attachment, 0.5}, {Integration, 0.3}}, "home": {{Attachment, 0.5}},
	"food": {{Attachment, 0.4}, {Release, 0.2}}, "cooking": {{Attachment, 0.4}},
	// Disruption: the trickster.
	"meme": {{Disruption, 0.8}}, "memes": {{Disruption, 0.8}}, "shitpost": {{Disruption, 0.9}},
	"chaos": {{Disruption, 0.8}}, "random": {{Disruption, 0.7}}, "lol": {{Disruption, 0.6}, {Release, 0.3}},
	"funny": {{Disruption, 0.6}, {Release, 0.3}}, "humor": {{Disruption, 0.6}, {Release, 0.3}},
	"humour": {{Disruption, 0.6}, {Release, 0.3}}, "weird": {{Disruption, 0.8}}, "cursed": {{Disruption, 0.8}},
	"experimental": {{Disruption, 0.7}}, "glitch": {{Disruption, 0.6}}, "remix": {{Disruption, 0.5}, {Persona, 0.3}},
	"crypto": {{Disruption, 0.4}, {Agency, 0.3}, {Tension, 0.2}}, "web3": {{Disruption, 0.4}, {Agency, 0.3}},
	"hottake": {{Disruption, 0.8}, {Tension, 0.5}}, "unpopularopinion": {{Disruption, 0.8}, {Tension, 0.5}},
	// Tension: the contested.
	"politics": {{Tension, 0.8}}, "debate": {{Tension, 0.8}}, "drama": {{Tension, 0.7}, {Disruption, 0.3}},
	"controversy": {{Tension, 0.8}}, "rant": {{Tension, 0.7}, {Shadow, 0.3}}, "news": {{Tension, 0.4}},
	"breaking": {{Tension, 0.6}, {Disruption, 0.3}}, "election": {{Tension, 0.7}}, "protest": {{Tension, 0.7}, {Agency, 0.3}},
	"discourse": {{Tension, 0.6}}, "rivalry": {{Tension, 0.7}, {Agency, 0.3}}, "shadowban": {{Tension, 0.7}},
	"scam": {{Tension, 0.7}}, "warning": {{Tension, 0.7}}, "psa": {{Tension, 0.5}},
	// Release: the payoff.
	"celebration": {{Release, 0.8}}, "party": {{Release, 0.8}}, "weekend": {{Release, 0.6}},
	"friday": {{Release, 0.6}}, "vacation": {{Release, 0.7}}, "travel": {{Release, 0.5}, {Disruption, 0.3}},
	"wanderlust": {{Release, 0.5}, {Disruption, 0.3}}, "adventure": {{Release, 0.4}, {Agency, 0.4}, {Disruption, 0.3}},
	"newmusic": {{Release, 0.6}, {Persona, 0.5}}, "release": {{Release, 0.7}, {Persona, 0.4}},
	"drop": {{Release, 0.7}, {Persona, 0.3}}, "win": {{Release, 0.7}, {Agency, 0.4}},
	"graduation": {{Release, 0.8}, {Attachment, 0.3}}, "milestone": {{Release, 0.7}, {Agency, 0.3}},
	"happy": {{Release, 0.7}}, "joy": {{Release, 0.7}}, "summer": {{Release, 0.6}},
	"festival": {{Release, 0.7}, {Attachment, 0.3}}, "concert": {{Release, 0.6}, {Attachment, 0.3}},
	"goals": {{Release, 0.4}, {Agency, 0.5}},
}
