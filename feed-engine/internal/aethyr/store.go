// Package aethyr — in-memory content store + fallback ranker.
// In production, replace with calls to your content-service.
package aethyr

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/f33d3r/feed-engine/internal/model"
)

// ── Mock content store ────────────────────────────────────────────────────────
// Generates realistic content candidates for testing without a real content DB.
// Replace GetCandidates() with a content-service call in production.

var authors = []struct {
	Name   string
	Handle string
}{
	{"Nyx", "nyx_builds"},
	{"Solène Drai", "solene_d"},
	{"Kai Morrow", "kaimorrow"},
	{"Reyes", "reyes_arc"},
	{"Mika Tanaka", "mika_t"},
	{"Luca Voss", "lucavoss"},
	{"Amara Osei", "amara_osei"},
	{"Dev Patel", "devpatel_"},
	{"Zara Wu", "zarawu"},
	{"Felix Kern", "felixkern"},
}

var samplePosts = []string{
	"The system doesn't show you what you want — it shows you what you're becoming. That gap is where the algorithm lives.",
	"Three months of silence. Then one post hits 40k. Velocity is not linear. Stop optimising for consistency.",
	"Your shadow isn't what you hide from others. It's what you hide from yourself. Build with that.",
	"Latency is a design choice, not a constraint. Every 100ms above target is a product decision you made.",
	"The best feeds don't feel algorithmic. They feel like someone who understands you.",
	"Cold start isn't a bug. It's the most honest state of the system. Pure alignment. No history. No bias.",
	"I've been tracking my own engagement patterns for 60 days. The entropy peaks at 11pm. Every time.",
	"What TikTok understood that Twitter never did: completion is the signal, not the click.",
	"The hardest part of building a recommendation system is deciding what to recommend against.",
	"Depth beats frequency every time. One post people return to is worth 50 people scroll past.",
	"Ran the numbers on shadow scoring. New creators get suppressed by 40% in the first 48 hours. That's a choice.",
	"The Jung layer isn't decoration. It's the only honest model of why people engage with what they engage with.",
	"Exploration slots are the conscience of the algorithm. 2 per page. Not negotiable.",
	"Revenue and engagement are not opposed. Aligned incentives produce better content markets. Full stop.",
	"Built something today that would have taken me 3 months two years ago. The tooling changed. The thinking didn't.",
}

var tags = [][]string{
	{"tech", "ml"},
	{"design", "ui"},
	{"philosophy", "systems"},
	{"engineering", "backend"},
	{"culture", "media"},
	{"creator", "monetisation"},
}

var musicBodies = []string{
	"New drop: lo-fi jazz with a dark underbelly. 3am studio session. The Rhodes is running through the algorithm.",
	"Posted the stems for 'Dissolution'. Download and remix — tag me when you drop it.",
	"60 seconds of whatever this is. AethyrRank called it 'Attachment + Release'. I'll take it.",
	"Beat tape vol.2 is done. 14 tracks. No features. Just texture and movement.",
	"Why does every music algorithm think I want upbeat? Give me minor keys and slow the BPM.",
	"Finally nailed the mix on this one. Bass hits at 2:14. Headphones required.",
	"Ambient set from last Tuesday. Two hours. No tracklist. Pure vibes.",
	"Uploaded my first EP to Zior. The 8-axis breakdown is unsettling in the best way.",
	"Produced this entirely in headphones on a bus. Sometimes constraints are the feature.",
	"The shadow axis is carrying this whole release. Intentional or not — you decide.",
}

var musicTags = [][]string{
	{"music", "lo-fi", "jazz"},
	{"music", "stems", "remix"},
	{"audio", "ambient"},
	{"music", "beattape", "producer"},
	{"music", "algorithm"},
	{"audio", "mixing"},
	{"music", "ambient", "set"},
	{"audio", "ep", "zior"},
	{"music", "producer"},
	{"audio", "shadow"},
}

var exploreAuthors = []struct{ Name, Handle string }{
	{"Theo Bright", "theobright"}, {"Mara Singh", "marasingh"},
	{"Olu Adeyemi", "olu_creates"}, {"Cassidy Lane", "casslane"},
	{"Priya Nair", "priyanair_"}, {"Bastian Wolf", "bastianwolf"},
	{"Juno Park", "junopark"}, {"Ash Chen", "ashchen_"},
	{"Rivka Saar", "rivkasaar"}, {"Cole Mercer", "colemercer"},
}

var exploreBodies = []string{
	"Hot take: the creator economy is just freelancing with better marketing. Change my mind.",
	"Three years building in public. The best thing it did wasn't grow my audience — it changed my thinking.",
	"The metaverse flopped because it solved a problem nobody had. We have bodies. We like them.",
	"Every 'alternative' social platform looks exactly like the one it's replacing. Ship the defaults.",
	"Watched someone go from 0 to 100k in 8 weeks. Content was fine. The consistency was extraordinary.",
	"Real talk: most 'build in public' accounts are building personas, not products.",
	"The algorithm knows you're making content for the algorithm. That's why it underperforms.",
	"Found 3 brilliant creators with under 2k followers today. The ranking systems are still broken.",
	"Power law distributions in creator income are not a bug. They're the feature. That's the game.",
	"The platform you're building on doesn't care about you. The community you build does.",
	"Every platform that tried to solve monetisation first failed. Build the network first.",
	"Shadow distribution is real. New creators get 40% less reach in the first 30 days. That's policy.",
	"The best content I read this month came from accounts with no verification, no follower count shown.",
	"What if the chronological feed was better all along and we were gaslit for 8 years?",
	"Niche down until it hurts, then niche down again. The riches are in the niches everyone skipped.",
}

var exploreTags = [][]string{
	{"culture", "hottake"}, {"buildingpublic"}, {"web3"},
	{"product", "design"}, {"creator", "growth"}, {"buildingpublic"},
	{"algorithm", "creator"}, {"discovery"}, {"creatoreconomy"},
	{"platform"}, {"monetisation"}, {"shadowban"},
	{"discovery"}, {"chrono"}, {"niche"},
}

// GetCandidatesForSurface returns mock candidates appropriate for the given surface.
func GetCandidatesForSurface(surface, userID string, n int) []*model.Post {
	switch surface {
	case "music":
		return getMusicCandidates(n)
	case "explore":
		return getExploreCandidates(n)
	default:
		return GetCandidates(userID, n)
	}
}

func getExploreCandidates(n int) []*model.Post {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	posts := make([]*model.Post, n)
	for i := 0; i < n; i++ {
		author := exploreAuthors[i%len(exploreAuthors)]
		body   := exploreBodies[i%len(exploreBodies)]
		tagSet := exploreTags[i%len(exploreTags)]
		age    := time.Duration(rng.Intn(96)) * time.Hour

		impressions := rng.Intn(80000) + 500
		likes       := int(float64(impressions) * (0.01 + rng.Float64()*0.08))

		posts[i] = &model.Post{
			ID:              fmt.Sprintf("explore_%d_%d", i, time.Now().UnixNano()),
			ContentID:       fmt.Sprintf("e_%d", i),
			AuthorID:        fmt.Sprintf("xauthor_%d", i%len(exploreAuthors)),
			AuthorName:      author.Name,
			AuthorHandle:    author.Handle,
			AvatarSeed:      author.Handle,
			Body:            body,
			CreatedAt:       time.Now().Add(-age),
			Likes:           likes,
			Reposts:         likes / 3,
			Comments:        likes / 5,
			Saves:           likes / 10,
			ViewTimeSeconds: 20 + rng.Float64()*60,
			Impressions:     impressions,
			Tags:            tagSet,
			ContentType:     "text",
		}
	}
	return posts
}

func getMusicCandidates(n int) []*model.Post {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	posts := make([]*model.Post, n)
	for i := 0; i < n; i++ {
		author := authors[i%len(authors)]
		body   := musicBodies[i%len(musicBodies)]
		tagSet := musicTags[i%len(musicTags)]
		age    := time.Duration(rng.Intn(48)) * time.Hour

		impressions := rng.Intn(20000) + 100
		likes       := int(float64(impressions) * (0.03 + rng.Float64()*0.15))

		posts[i] = &model.Post{
			ID:              fmt.Sprintf("music_%d_%d", i, time.Now().UnixNano()),
			ContentID:       fmt.Sprintf("m_%d", i),
			AuthorID:        fmt.Sprintf("author_%d", i%len(authors)),
			AuthorName:      author.Name,
			AuthorHandle:    author.Handle,
			AvatarSeed:      author.Handle,
			Body:            body,
			CreatedAt:       time.Now().Add(-age),
			Likes:           likes,
			Reposts:         likes / 5,
			Comments:        likes / 7,
			Saves:           likes / 3,
			ViewTimeSeconds: 60 + rng.Float64()*180,
			Impressions:     impressions,
			Tags:            tagSet,
			ContentType:     "audio",
		}
	}
	return posts
}

// GetCandidates returns mock content candidates.
// In production: replace with content-service call filtered by user interests.
func GetCandidates(userID string, n int) []*model.Post {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	posts := make([]*model.Post, n)
	for i := 0; i < n; i++ {
		author := authors[i%len(authors)]
		body   := samplePosts[i%len(samplePosts)]
		tagSet := tags[i%len(tags)]
		age    := time.Duration(rng.Intn(72)) * time.Hour

		impressions := rng.Intn(50000) + 100
		likes       := int(float64(impressions) * (0.02 + rng.Float64()*0.12))

		posts[i] = &model.Post{
			ID:              fmt.Sprintf("post_%d_%d", i, time.Now().UnixNano()),
			ContentID:       fmt.Sprintf("c_%d", i),
			AuthorID:        fmt.Sprintf("author_%d", i%len(authors)),
			AuthorName:      author.Name,
			AuthorHandle:    author.Handle,
			AvatarSeed:      author.Handle,
			Body:            body,
			CreatedAt:       time.Now().Add(-age),
			Likes:           likes,
			Reposts:         likes / 4,
			Comments:        likes / 6,
			Saves:           likes / 8,
			ViewTimeSeconds: 30 + rng.Float64()*90,
			Impressions:     impressions,
			Tags:            tagSet,
			ContentType:     "text",
		}
	}
	return posts
}

// PostToAethyrContent converts a Post to the AethyrRank content wire type.
func PostToAethyrContent(p *model.Post) model.AethyrContent {
	age := time.Since(p.CreatedAt).Hours()

	// Freshness proxy
	freshnessScore := math.Exp(-0.693 / 24.0 * age)

	// Velocity: prefer real-time signal from feedback_events (set by feedPartial),
	// fall back to static post_metrics when no live data is available yet.
	var velocityScore float64
	if p.FeedVelocity > 0 {
		velocityScore = p.FeedVelocity
	} else if p.Impressions > 0 {
		rate := float64(p.Likes+p.Reposts*3+p.Comments*2+p.Saves*4) / float64(p.Impressions)
		velocityScore = 1.0 / (1.0 + math.Exp(-rate*20.0))
	}

	// Topic vector: deterministic from author seed (8 dims)
	topicVec := SeedVector(p.AuthorID, 8)

	return model.AethyrContent{
		ContentID:             p.ContentID,
		CreatorID:             p.AuthorID,
		TopicVector:           topicVec,
		PublishedAt:           p.CreatedAt,
		ExposureCount:         uint64(p.Impressions),
		CreatorExposure:       uint64(p.Impressions * 3),
		Tags:                  p.Tags,
		ContentType:           p.ContentType,
		VelocityScore:         velocityScore,
		EarlyRetention:        freshnessScore * 0.7,
		CompletionRate:        freshnessScore * 0.6,
		ConversionProbability: 0.05,
		CreatorRevenueRate:    0.20,
		LtvEstimate:           0.15,
		AdultProbability:      0.0,
		PostsLast24h:          0,   // patched in buildRankRequest via BatchGetCreatorPostCounts24h
		SelfReplyCadence:      0.0, // patched in buildRankRequest via BatchGetSelfReplyFirst30m
		CharCount:             uint64(len([]rune(p.Body))),
		Engagement: model.AethyrEngagement{
			Likes:           float64(p.Likes),
			Shares:          float64(p.Reposts),
			Comments:        float64(p.Comments),
			Saves:           float64(p.Saves),
			ViewTimeSeconds: p.ViewTimeSeconds,
			Impressions:     float64(p.Impressions),
		},
	}
}

// FallbackRankPosts sorts posts by engagement×freshness when AethyrRank is unavailable.
func FallbackRankPosts(posts []*model.Post) []*model.Post {
	sort.Slice(posts, func(i, j int) bool {
		si := fallbackScore(posts[i])
		sj := fallbackScore(posts[j])
		return si > sj
	})
	return posts
}

func fallbackScore(p *model.Post) float64 {
	age := time.Since(p.CreatedAt).Hours()
	freshness := math.Exp(-0.693 / 24.0 * age)
	if p.Impressions == 0 {
		// No impression data yet — score by freshness alone so new posts surface.
		return freshness
	}
	eng := float64(p.Likes+p.Reposts*3+p.Comments*2+p.Saves*4) / float64(p.Impressions)
	return eng * freshness
}

// SeedVector produces a deterministic normalised float32 vector from a string seed.
// Exported so the handler layer can compute implicit interest vectors from author IDs.
func SeedVector(seed string, dim int) []float32 {
	h := uint64(14695981039346656037)
	for _, c := range seed {
		h ^= uint64(c)
		h *= 1099511628211
	}
	vec := make([]float32, dim)
	var norm float32
	for i := range vec {
		h ^= h >> 33
		h *= 0xff51afd7ed558ccd
		h ^= h >> 33
		v := float32(h&0xFFFF) / float32(0xFFFF)
		vec[i] = v
		norm += v * v
	}
	norm = float32(math.Sqrt(float64(norm)))
	if norm > 0 {
		for i := range vec {
			vec[i] /= norm
		}
	}
	return vec
}
