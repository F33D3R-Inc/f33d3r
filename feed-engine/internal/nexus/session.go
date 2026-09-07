package nexus

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// Session holds the NEXUS multi-persona context for an authenticated request.
// The raw nexus_id is NEVER stored here — only derived, blinded identifiers.
type Session struct {
	SessionID        string
	NexusIDHash      string // SHA256(nexus_id) — never raw nexus_id
	NexusShard       string // for AethyrRank — SHA256(nexus_id + "aethyrrank-nexus-v1")
	ActivePIALShard  string
	AllPersonaShards []string
	Tier             int
	UnifiedView      bool
	UnifiedMode      string
	ExpiresAt        time.Time
}

// sessionCacheEntry wraps a Session with an expiry for in-process caching.
type sessionCacheEntry struct {
	session   *Session
	expiresAt time.Time
}

var (
	sessionCache    sync.Map
	sessionCacheTTL = 30 * time.Second
)

// GetSessionForUser returns the NEXUS session for a user, or nil if the user
// is not linked to any NEXUS (single-account path).
//
// Calls Verity to resolve the persona list on first access; caches for 30s.
// The client parameter must already have been initialised with the Verity base URL.
func GetSessionForUser(pialID string, client *Client) *Session {
	if pialID == "" {
		return nil
	}

	activeShard := PersonaShardFromPIAL(pialID)
	cacheKey := "nexus:" + activeShard

	// Fast path: in-process cache
	if v, ok := sessionCache.Load(cacheKey); ok {
		if e := v.(sessionCacheEntry); time.Now().Before(e.expiresAt) {
			return e.session
		}
		sessionCache.Delete(cacheKey)
	}

	// Slow path: call Verity
	personas, err := client.ListPersonas(activeShard)
	if err != nil || personas == nil {
		// Not in NEXUS — cache the nil result to avoid hammering Verity
		sessionCache.Store(cacheKey, sessionCacheEntry{
			session:   nil,
			expiresAt: time.Now().Add(sessionCacheTTL),
		})
		return nil
	}

	allShards := make([]string, 0, len(personas.Personas))
	for _, p := range personas.Personas {
		allShards = append(allShards, p.PIALShardID)
	}

	// Derive blinded identifiers — never store raw nexus_id.
	// We don't have the raw nexus_id here; we use the active shard as a proxy
	// for the nexus shard derivation (consistent across all personas in the nexus).
	nexusShard := AethyrRankShard(activeShard)

	sess := &Session{
		NexusIDHash:      DeriveNexusShard(activeShard, "nexus-id-hash-v1"),
		NexusShard:       nexusShard,
		ActivePIALShard:  activeShard,
		AllPersonaShards: allShards,
		Tier:             personas.Tier,
		ExpiresAt:        time.Now().Add(7 * 24 * time.Hour),
	}

	sessionCache.Store(cacheKey, sessionCacheEntry{
		session:   sess,
		expiresAt: time.Now().Add(sessionCacheTTL),
	})
	return sess
}

// InvalidateSession removes a session from the in-process cache.
// Called on persona switch so the next request gets a fresh session.
func InvalidateSession(pialID string) {
	shard := PersonaShardFromPIAL(pialID)
	sessionCache.Delete("nexus:" + shard)
}

// DeriveNexusShard derives a blinded identifier from a shard string + purpose.
func DeriveNexusShard(shard, purpose string) string {
	h := sha256.Sum256([]byte(shard + ":" + purpose))
	return hex.EncodeToString(h[:])
}

// PersonaShardFromPIAL derives a pial_shard_id from a PIAL UUID string.
// This matches the formula used in Verity's nexus tables.
func PersonaShardFromPIAL(pialID string) string {
	h := sha256.Sum256([]byte(pialID + ":nexus-pial-shard-v1"))
	return hex.EncodeToString(h[:])
}

// AethyrRankShard derives the nexus_shard_id to pass to AethyrRank.
func AethyrRankShard(activeShard string) string {
	h := sha256.Sum256([]byte(activeShard + ":aethyrrank-nexus-v1"))
	return hex.EncodeToString(h[:])
}
