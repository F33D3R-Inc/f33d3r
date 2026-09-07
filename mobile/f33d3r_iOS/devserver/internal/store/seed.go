package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Seed is the dev deployment's starting state: the same five accounts
// CLAUDE.md lists with the same password, a follow graph between them, a
// starting balance each, and a handful of works so every lane has something
// to draw on first launch.
//
// Idempotent: it runs on every start and does nothing once @dev exists. Every
// row it writes is an ordinary row — a seeded work is deleted, liked and
// replied to like any other, and its CID is computed from a real canonical
// payload rather than invented.

// DevPassword is the password every seeded account uses.
const DevPassword = "f33d3rdev"

type seedAccount struct {
	NewUserParams
	follows []string
}

var seedAccounts = []seedAccount{
	{NewUserParams{Handle: "tehanibentley", DisplayName: "Tehani Bentley", Role: "founder", Tier: "creator", IsCreator: true, IsVerified: true, XP: 24000, KYCTier: "full",
		Bio: "Founder, F33D3R. Your identity is yours, your money goes straight to you.", Location: "Los Angeles", Website: "https://f33d3r.com", AccentHex: "#7c5cff"},
		[]string{"admin", "miiyazuko", "dev"}},
	{NewUserParams{Handle: "admin", DisplayName: "Admin", Role: "admin", IsVerified: true, XP: 20500, Bio: "Keeps the lights on."},
		[]string{"tehanibentley", "dev"}},
	{NewUserParams{Handle: "miiyazuko", DisplayName: "Mii Yazuko", Tier: "creator", IsCreator: true, IsVerified: true, XP: 8200, KYCTier: "full",
		Bio: "Film photographer. 35mm, nothing retouched.", Pronouns: "she/her", Location: "Tokyo", AccentHex: "#e05080"},
		[]string{"tehanibentley", "dev"}},
	{NewUserParams{Handle: "dev", DisplayName: "Dev Account", Tier: "creator", IsCreator: true, IsVerified: true, XP: 2500, KYCTier: "full",
		Bio: "Engineer at F33D3R. Building the thing.", Pronouns: "they/them", Location: "Portland, OR", Website: "https://example.com", AccentHex: "#7c5cff"},
		[]string{"tehanibentley", "miiyazuko", "admin"}},
	{NewUserParams{Handle: "guest", DisplayName: "Guest", XP: 120, Bio: "Just looking."},
		[]string{"tehanibentley"}},
}

type seedWork struct {
	handle  string
	body    string
	kind    string
	tags    []string
	media   []string
	poll    []string
	pollEnd time.Duration
	voice   string
	voiceS  int
	// price sells the work outright, in µAET. Zero is not for sale.
	price   int64
	age     time.Duration
	replyTo int // index into seedWorks, -1 for none
	// quoteOf is the work this one quotes, an index into seedWorks, -1 for
	// none. Same shape as replyTo and for the same reason: a citation names a
	// work that has to exist first, so the insert order below is a dependency
	// order over both fields.
	quoteOf int
}

var seedWorks = []seedWork{
	{handle: "tehanibentley", body: "Shipped the native client's first screen tonight. Same tokens as the web, same card, no framework in sight.", age: 26 * time.Hour, replyTo: -1, quoteOf: -1},
	{handle: "miiyazuko", body: "four frames from the roll, nothing retouched #film #35mm", tags: []string{"film", "35mm"},
		media: []string{"/media/seed/roll-01.png", "/media/seed/roll-02.png", "/media/seed/roll-03.png", "/media/seed/roll-04.png"}, age: 22 * time.Hour, replyTo: -1, quoteOf: -1},
	{handle: "dev", body: "Reposts show the poster in the header and the original creator underneath. Never both in the same slot.", age: 20 * time.Hour, replyTo: -1, quoteOf: -1},
	{handle: "admin", body: "Which lane should open by default?", kind: "poll", poll: []string{"Following", "For you", "Whatever I left it on"}, pollEnd: 36 * time.Hour, age: 18 * time.Hour, replyTo: -1, quoteOf: -1},
	{handle: "miiyazuko", body: "Late set from the rooftop. Room mic a little high. #music", tags: []string{"music"}, kind: "voice", voice: "/media/seed/rooftop.m4a", voiceS: 214, age: 15 * time.Hour, replyTo: -1, quoteOf: -1},
	{handle: "dev", body: "Agreed — and the badge should say who the *creator* is, not who reposted.", kind: "reply", age: 19 * time.Hour, replyTo: 2, quoteOf: -1},
	{handle: "guest", body: "First post. Hello from the other side of the glass.", age: 6 * time.Hour, replyTo: -1, quoteOf: -1},
	{handle: "tehanibentley", body: "Tips settle straight to the creator's wallet. We never hold the money. That's the whole point. #f33d3r", tags: []string{"f33d3r"}, age: 3 * time.Hour, replyTo: -1, quoteOf: -1},
	{handle: "dev", body: "This one's the one to look at if you're wondering how the card handles a photo grid.", kind: "reply", age: 21 * time.Hour, replyTo: 1, quoteOf: -1},
	{handle: "miiyazuko", body: "one more from the same roll, the light did the work", media: []string{"/media/seed/roll-05.png"}, age: 40 * time.Minute, replyTo: -1, quoteOf: -1},
	// Two tracks sold outright, so the Music store has rows and a purchase can
	// be exercised end to end. Appended rather than inserted so the indices the
	// engagement below refers to stay put.
	{handle: "miiyazuko", body: "Rooftop, take two\nThe clean take. Yours to keep. #music", tags: []string{"music"}, kind: "voice",
		voice: "/media/seed/rooftop.m4a", voiceS: 214, price: 2 * UAETPerAET, media: []string{"/media/seed/roll-02.png"}, age: 9 * time.Hour, replyTo: -1, quoteOf: -1},
	{handle: "tehanibentley", body: "Golden Hour (single)\nFirst release on F33D3R. Paid straight to me, no label in between. #music", tags: []string{"music"}, kind: "voice",
		voice: "/media/seed/rooftop.m4a", voiceS: 214, price: 5 * UAETPerAET, media: []string{"/media/seed/roll-04.png"}, age: 5 * time.Hour, replyTo: -1, quoteOf: -1},
	// A quote chain three deep, so the card has a real one to draw: the work
	// itself, the bordered card of the work it quotes, and the compact rail
	// under that for the work *that* one quotes. Index 1 is the four-frame
	// roll, which is what puts a "+3" on the rail's thumbnail — the deepest
	// level is the one carrying more than one image, and that is the case the
	// overflow badge exists for.
	{handle: "tehanibentley", body: "I don't follow film photography but this is the best thing anyone has posted here all week.",
		kind: "quote", age: 2 * time.Hour, replyTo: -1, quoteOf: 1},
	{handle: "dev", body: "Like that", kind: "quote", age: 80 * time.Minute, replyTo: -1, quoteOf: 12},
}

// SeedIfEmpty populates a fresh database. It is a no-op once @dev exists.
func (s *Store) SeedIfEmpty(ctx context.Context) (bool, error) {
	if taken, err := s.IsHandleTaken(ctx, "dev"); err != nil || taken {
		return false, err
	}
	base := time.Now().Add(-48 * time.Hour)
	users := map[string]*User{}
	for i, a := range seedAccounts {
		a.Password = DevPassword
		created := base.Add(time.Duration(i) * time.Minute)
		a.CreatedAt = &created
		u, err := s.CreateUser(ctx, a.NewUserParams)
		if err != nil {
			return false, fmt.Errorf("seed user %s: %w", a.Handle, err)
		}
		users[a.Handle] = u
		// Everyone starts with 50 AET so tipping can be exercised.
		if err := s.Credit(ctx, u.PIALID, "airdrop", 50*UAETPerAET, created); err != nil {
			return false, err
		}
	}
	for _, a := range seedAccounts {
		for _, target := range a.follows {
			if _, err := s.Follow(ctx, users[a.Handle].ID, users[target].ID); err != nil {
				return false, err
			}
		}
	}

	ids := make([]string, len(seedWorks))
	cids := make([]string, len(seedWorks))
	// Parents must exist before replies cite them: insert in dependency order.
	order := make([]int, 0, len(seedWorks))
	done := map[int]bool{}
	for len(order) < len(seedWorks) {
		for i, w := range seedWorks {
			if done[i] || (w.replyTo >= 0 && !done[w.replyTo]) || (w.quoteOf >= 0 && !done[w.quoteOf]) {
				continue
			}
			order = append(order, i)
			done[i] = true
		}
	}
	for _, i := range order {
		w := seedWorks[i]
		u := users[w.handle]
		kind := w.kind
		if kind == "" {
			kind = "post"
		}
		created := time.Now().Add(-w.age)
		parentCID := ""
		if w.replyTo >= 0 {
			parentCID = cids[w.replyTo]
		}
		quotedCID := ""
		if w.quoteOf >= 0 {
			quotedCID = cids[w.quoteOf]
		}
		var pollEnds *time.Time
		if w.poll != nil {
			t := created.Add(w.pollEnd)
			pollEnds = &t
		}
		var voiceSecs *int
		if w.voice != "" {
			v := w.voiceS
			voiceSecs = &v
		}
		cid := seedCID(u.PIALID, w, kind, parentCID, pollEnds, created)
		id, err := s.InsertWork(ctx, InsertWorkParams{
			AuthorID: u.ID, AuthorPIAL: u.PIALID, CID: cid, Body: w.body, Kind: kind,
			MediaURLs: w.media, ParentCID: parentCID, QuotedCID: quotedCID, Tags: w.tags,
			PollOptions: w.poll, PollEndsAt: pollEnds, CommentGating: "open",
			VoiceURL: w.voice, VoiceDurationSecs: voiceSecs, PriceUAET: w.price, CreatedAt: created,
		})
		if err != nil {
			return false, fmt.Errorf("seed work %d: %w", i, err)
		}
		ids[i] = id
		cids[i] = cid
	}

	// Some engagement so counts and the Trending lane are non-trivial.
	react := func(handle string, work int, kind string) {
		s.React(ctx, ids[work], users[handle].ID, kind, true)
	}
	react("dev", 0, "like")
	react("miiyazuko", 0, "like")
	react("admin", 0, "like")
	react("guest", 0, "like")
	react("admin", 0, "repost")
	react("dev", 1, "like")
	react("tehanibentley", 1, "like")
	react("dev", 1, "bookmark")
	react("tehanibentley", 2, "repost")
	react("guest", 7, "like")
	react("dev", 7, "like")
	react("miiyazuko", 7, "repost")
	react("dev", 12, "like")
	react("miiyazuko", 12, "like")
	react("guest", 13, "like")
	react("tehanibentley", 13, "like")
	react("miiyazuko", 13, "bookmark")
	s.CastPollVote(ctx, ids[3], users["dev"].ID, 2)
	s.CastPollVote(ctx, ids[3], users["miiyazuko"].ID, 0)
	s.CastPollVote(ctx, ids[3], users["guest"].ID, 2)
	s.Tip(ctx, users["dev"].PIALID, users["miiyazuko"].PIALID, ids[1], 2*UAETPerAET+500_000)
	s.Tip(ctx, users["guest"].PIALID, users["tehanibentley"].PIALID, ids[7], UAETPerAET)

	// Notifications the engagement above owes.
	s.Notify(ctx, users["tehanibentley"].ID, "like", users["dev"].ID, ids[0], "work", map[string]any{"preview": seedWorks[0].body})
	s.Notify(ctx, users["tehanibentley"].ID, "like", users["miiyazuko"].ID, ids[0], "work", map[string]any{"preview": seedWorks[0].body})
	s.Notify(ctx, users["tehanibentley"].ID, "like", users["admin"].ID, ids[0], "work", map[string]any{"preview": seedWorks[0].body})
	s.Notify(ctx, users["tehanibentley"].ID, "repost", users["admin"].ID, ids[0], "work", map[string]any{"preview": seedWorks[0].body})
	s.Notify(ctx, users["miiyazuko"].ID, "like", users["dev"].ID, ids[1], "work", map[string]any{"preview": seedWorks[1].body})
	s.Notify(ctx, users["miiyazuko"].ID, "tip", users["dev"].ID, ids[1], "work", map[string]any{"amount_uaet": 2*UAETPerAET + 500_000})
	s.Notify(ctx, users["miiyazuko"].ID, "reply", users["dev"].ID, ids[8], "work", map[string]any{"preview": seedWorks[8].body})
	s.Notify(ctx, users["dev"].ID, "reply", users["dev"].ID, ids[5], "work", nil) // self: dropped by Notify
	s.Notify(ctx, users["dev"].ID, "repost", users["tehanibentley"].ID, ids[2], "work", map[string]any{"preview": seedWorks[2].body})
	s.Notify(ctx, users["dev"].ID, "follow", users["tehanibentley"].ID, "", "profile", nil)
	s.Notify(ctx, users["dev"].ID, "follow", users["admin"].ID, "", "profile", nil)
	s.Notify(ctx, users["dev"].ID, "follow", users["miiyazuko"].ID, "", "profile", nil)
	s.Notify(ctx, users["tehanibentley"].ID, "tip", users["guest"].ID, ids[7], "work", map[string]any{"amount_uaet": UAETPerAET})
	s.Notify(ctx, users["miiyazuko"].ID, "quote", users["tehanibentley"].ID, ids[12], "work", map[string]any{"preview": seedWorks[12].body})
	s.Notify(ctx, users["tehanibentley"].ID, "quote", users["dev"].ID, ids[13], "work", map[string]any{"preview": seedWorks[13].body})

	// Visions: one text, one photo, one poll — so the tray has three rings and
	// the viewer has every shape to draw.
	s.InsertVision(ctx, VisionInput{
		AuthorID: users["tehanibentley"].ID, AuthorPIAL: users["tehanibentley"].PIALID,
		ContentType: "text", Body: "Native client ships this week. Same card everywhere.", Background: "ember",
		Typeface: "display", AllowReplies: true, CreatedAt: time.Now().Add(-3 * time.Hour),
	})
	s.InsertVision(ctx, VisionInput{
		AuthorID: users["tehanibentley"].ID, AuthorPIAL: users["tehanibentley"].PIALID,
		ContentType: "text", Body: "Tips settle to you. We never hold the money.", Background: "void",
		Typeface: "grotesk", AllowReplies: true, CreatedAt: time.Now().Add(-70 * time.Minute),
	})
	s.InsertVision(ctx, VisionInput{
		AuthorID: users["miiyazuko"].ID, AuthorPIAL: users["miiyazuko"].PIALID,
		ContentType: "image", Body: "from the rooftop roll", MediaURLs: []string{"/media/seed/roll-03.png"},
		AllowReplies: true, CreatedAt: time.Now().Add(-5 * time.Hour),
	})
	s.InsertVision(ctx, VisionInput{
		AuthorID: users["admin"].ID, AuthorPIAL: users["admin"].PIALID,
		ContentType: "text", Body: "Which cut?", Background: "tide", Typeface: "mono",
		PollOptions: []string{"Rooftop", "Studio"}, AllowReplies: false, CreatedAt: time.Now().Add(-40 * time.Minute),
	})

	// A live room, so the Live lane, the viewer and the broadcaster have a
	// real row to draw on first launch.
	if room, err := s.StartLive(ctx, LiveInput{
		AuthorID: users["tehanibentley"].ID, AuthorPIAL: users["tehanibentley"].PIALID,
		Title: "Late set from the rooftop", Description: "Playing the new one at 200 AET.",
		Audience: "everyone", Lane: "music", TipGoalUAET: 200 * UAETPerAET, Notify: true, SaveReplay: true,
	}); err == nil {
		s.PinLive(ctx, room.ID, "new track drops at 200 AET")
		s.AppendChat(ctx, room.ID, users["miiyazuko"], "chat", "play the new one", 0)
		s.AppendChat(ctx, room.ID, users["guest"], "chat", "this is it", 0)
		s.TipStream(ctx, users["admin"].PIALID, users["tehanibentley"].PIALID, room.ID, 40*UAETPerAET)
		s.AppendChat(ctx, room.ID, users["admin"], "tip", "", 40*UAETPerAET)
		s.AppendChat(ctx, room.ID, users["dev"], "chat", "room mic a little high but the mix is right", 0)
	}
	return true, nil
}

// seedCID hashes the canonical payload a client would have signed for this
// work, so seeded CIDs are real content ids and not placeholders. The field
// set and order are workCanonicalPayload's.
func seedCID(pial string, w seedWork, kind, parentCID string, pollEnds *time.Time, created time.Time) string {
	media := append([]string{}, w.media...)
	if media == nil {
		media = []string{}
	}
	sort.Strings(media)
	tags := w.tags
	if tags == nil {
		tags = []string{}
	}
	var parent, pollEndsAt, pollOptions, voiceURL, voiceDur any
	if parentCID != "" {
		parent = parentCID
	}
	if pollEnds != nil {
		pollEndsAt = pollEnds.UTC().Format(time.RFC3339)
	}
	if w.poll != nil {
		pollOptions = w.poll
	}
	if w.voice != "" {
		voiceURL = w.voice
		voiceDur = w.voiceS
	}
	payload := []struct {
		k string
		v any
	}{
		{"author_pial", pial}, {"body", w.body}, {"comment_gating", "everyone"}, {"is_repost", false}, {"kind", kind},
		{"media_urls", media}, {"parent_cid", parent}, {"poll_ends_at", pollEndsAt}, {"poll_options", pollOptions},
		{"repost_source_id", nil}, {"scheduled_at", nil}, {"subscriber_only", false}, {"tags", tags},
		{"timestamp_ms", created.UnixMilli()}, {"video_duration_secs", nil}, {"video_master_url", nil},
		{"video_poster_url", nil}, {"voice_duration_secs", voiceDur}, {"voice_url", voiceURL},
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range payload {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(f.k)
		buf.Write(k)
		buf.WriteByte(':')
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.Encode(f.v)
		buf.Truncate(buf.Len() - 1) // Encode's newline
	}
	buf.WriteByte('}')
	sum := sha256.Sum256(buf.Bytes())
	return fmt.Sprintf("sha256:%x", sum)
}
