package config

import (
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Port string
	Host string
	// Database
	DatabaseURL string
	// AethyrRank
	AethyrRankURL     string
	AethyrRankTimeout time.Duration
	// Ain-Soph wallet
	AinSophURL string
	// Zodacare (safety brain)
	ZodacareURL string
	// Elohim Veni (security brain)
	ElohimVeniURL string
	// Verity (KYC / compliance brain)
	VerityURL string
	// Aethyr Ledger (AET settlement fabric)
	LedgerURL string
	// Caeor (media brain — image resize, WebP conversion, derivatives)
	CaeorURL string
	// Themis — unified commerce + Bitcoin marketplace brain
	ThemisURL string
	// Transcoding brain — ffmpeg video processing (separate server in production)
	TranscodingURL string
	// Astraon — creator analytics brain (Rust, port 8088)
	AstraonURL string
	// Zior — audio brain (Rust, port 8082). Places a track or a voice work on
	// the eight Jung axes from the audio itself and blends listener behaviour
	// into it; for audio works its vector is authoritative over the text
	// mapper. Env: ZIOR_URL (compose passes http://zior-engine:8082).
	ZiorURL string
	// eKYC brain — face match, OCR, anti-spoof (Python, port 8099)
	EKYCUrl string
	// Alexandria — hypermedia library brain (content catalog)
	AlexandriaURL string
	// Herald push notification brain
	HeraldURL string
	// Manhattan — the naming plane. Resolves a public name to the node it
	// points at, and owns the edge graph. Every other brain reaches an entity
	// through here instead of joining into the table that stores it.
	ManhattanURL string
	// RegistryBrainURL — the handle authority. A handle is an allocation, not a
	// column: registration, transfer, auction and reclaim all belong to that
	// brain, and Manhattan refuses a handle binding published by anyone else.
	// Unset means handles cannot be allocated at all, which is a failure and
	// not a degraded mode.
	RegistryBrainURL string
	// SMTP — outbound email (password reset, notifications)
	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	SMTPFrom string
	// Azure Content Moderator — CSAM detection
	AzureCMEndpoint string
	AzureCMKey      string
	// Feed
	FeedPageSize      int
	FeedMaxCandidates int
	DefaultSurface    string
	// Platform base URL (for email links)
	BaseURL string
	// Internal service-to-service auth key
	InternalAPIKey string

	// Onion-Location. The .onion address is derived by tor from the
	// hidden-service key, so it is never written into a config file: the tor
	// service exports the hostname it derived to a file that this process reads
	// (OnionHostnameFile, env ONION_HOSTNAME_FILE, default /run/tor/hostname).
	// OnionHostname (env ONION_HOSTNAME) pins an address served by a tor daemon
	// outside the stack and takes precedence when set. tor starts after this
	// process, so the file may not exist at boot — OnionHost resolves it lazily.
	OnionHostname     string
	OnionHostnameFile string

	// Sports — live game cards. The lane is ALWAYS on and has no off switch:
	// SportsProvider is an optional override only. Blank resolves to Big Balls
	// when BBS_API is set and to ESPN's keyless public scoreboard otherwise, so
	// no env file edit can take the cards down. SportsAPIKey is read from
	// BBS_API — that is the one and only name for it. SportsBaseURL overrides
	// the upstream host for a test double or a self-hosted mirror.
	SportsProvider string
	SportsAPIKey   string
	SportsBaseURL  string
	// GIF search proxy — KLIPY (klipy.com), a distinct provider from Giphy/Tenor.
	// Key env: KLIPHY_API.
	KlipyAPIKey string
	// Media token — HMAC secret for NSFW media URL gating (D-005)
	MediaTokenSecret string
	// Tiingo financial data API — cashtag price cards and sparklines
	TiingoAPIKey string
	// Translation API — LibreTranslate-compatible endpoint
	TranslationAPIURL string
	TranslationAPIKey string
	// Security alerting — email sent on critical/high security events
	SecurityAlertEmail string
	// Sitra Achra — Kafka/Redpanda event backbone
	KafkaBrokers string
	// Auralis — the Frequencies (live audio) brain
	AuralisURL     string
	AuralisTimeout time.Duration
	// Dev
	DevMode    bool
	ShowScores bool
	// TemplateStrict turns on "missingkey=error" template parsing — a render that
	// references a data key its handler never populated becomes a clean 500
	// instead of silently emitting "<no value>". Opt-in (separate from DevMode)
	// because many handlers currently build inconsistent data maps; use it to
	// hunt that wiring drift, not for normal local browsing.
	TemplateStrict bool
}

func Load() *Config {
	c := &Config{
		Port:              getEnv("PORT", "8081"),
		Host:              getEnv("HOST", "0.0.0.0"),
		DatabaseURL:       getEnv("DATABASE_URL", ""),
		AethyrRankURL:     getEnv("AETHYRRANK_URL", "http://localhost:8080"),
		AethyrRankTimeout: getDurationEnv("AETHYRRANK_TIMEOUT_MS", 200) * time.Millisecond,
		AinSophURL:        getEnv("AIN_SOPH_URL", "http://localhost:8089"),
		ZodacareURL:       getEnv("ZODACARE_URL", "http://localhost:8090"),
		ElohimVeniURL:     getEnv("ELOHIM_VENI_URL", "http://localhost:8093"),
		VerityURL:         getEnv("VERITY_URL", "http://localhost:8095"),
		LedgerURL:         getEnv("LEDGER_URL", "http://localhost:8096"),
		CaeorURL:          getEnv("CAEOR_URL", "http://localhost:8086"),
		ThemisURL:         getEnv("THEMIS_URL", "http://localhost:8100"),
		TranscodingURL:    getEnv("TRANSCODING_URL", "http://transcoding:8085"),
		AstraonURL:        getEnv("ASTRAON_URL", "http://astraon:8088"),
		ZiorURL:           getEnv("ZIOR_URL", "http://localhost:8082"),
		EKYCUrl:           getEnv("EKYC_URL", "http://ekyc:8099"),
		AlexandriaURL:     getEnv("ALEXANDRIA_URL", "http://localhost:8098"),
		HeraldURL:         getEnv("HERALD_URL", ""),
		ManhattanURL:      getEnv("MANHATTAN_URL", ""),
		RegistryBrainURL:  getEnv("REGISTRY_BRAIN_URL", "http://registry-brain:8094"),
		SMTPHost:          getEnv("SMTP_HOST", ""),
		SMTPPort:          getIntEnv("SMTP_PORT", 587),
		SMTPUser:          getEnv("SMTP_USER", ""),
		SMTPPass:          getEnv("SMTP_PASS", ""),
		SMTPFrom:          getEnv("SMTP_FROM", "noreply@f33d3r.com"),
		AzureCMEndpoint:   getEnv("AZURE_CM_ENDPOINT", ""),
		AzureCMKey:        getEnv("AZURE_CM_KEY", ""),
		FeedPageSize:      getIntEnv("FEED_PAGE_SIZE", 20),
		FeedMaxCandidates: getIntEnv("FEED_MAX_CANDIDATES", 200),
		DefaultSurface:    getEnv("DEFAULT_SURFACE", "feed"),
		BaseURL:           getEnv("BASE_URL", "https://f33d3r.com"),
		InternalAPIKey:    getEnv("INTERNAL_API_KEY", ""),
		OnionHostname:     getEnv("ONION_HOSTNAME", ""),
		OnionHostnameFile: getEnv("ONION_HOSTNAME_FILE", "/run/tor/hostname"),
		SportsProvider:    getEnv("SPORTS_PROVIDER", ""),
		// The sports key is read from BBS_API — one variable, one name, named for
		// the one vendor that charges for this lane. Setting it selects Big Balls;
		// leaving it empty selects ESPN. The lane runs either way.
		SportsAPIKey:       getEnv("BBS_API", ""),
		SportsBaseURL:      getEnv("SPORTS_BASE_URL", ""),
		KlipyAPIKey:        getEnv("KLIPHY_API", ""),
		MediaTokenSecret:   getEnv("MEDIA_TOKEN_SECRET", ""),
		TiingoAPIKey:       getEnv("TIINGO_API_KEY", ""),
		TranslationAPIURL:  getEnv("TRANSLATION_API_URL", ""),
		TranslationAPIKey:  getEnv("TRANSLATION_API_KEY", ""),
		SecurityAlertEmail: getEnv("SECURITY_ALERT_EMAIL", ""),
		KafkaBrokers:       getEnv("KAFKA_BROKERS", ""),
		AuralisURL:         getEnv("AURALIS_URL", "http://auralis:8108"),
		AuralisTimeout:     getDurationEnv("AURALIS_TIMEOUT_MS", 3000) * time.Millisecond,
		DevMode:            getBoolEnv("DEV_MODE", false),
		ShowScores:         getBoolEnv("SHOW_SCORES", false),
		TemplateStrict:     getBoolEnv("TEMPLATE_STRICT", false),
	}
	log.Printf("[config] AethyrRank: %s | Themis: %s | DB: %s",
		c.AethyrRankURL, c.ThemisURL, sanitizeURL(c.DatabaseURL))
	if c.InternalAPIKey == "" {
		// Every brain-facing endpoint fails closed on an empty key and every
		// outbound brain call would be sent unauthenticated. Said here, once, at
		// the moment the configuration is read, so the cause is in the log
		// before the first refused request is.
		log.Printf("[config] INTERNAL_API_KEY is unset — internal endpoints will refuse every caller and brain calls will be rejected by their peers")
	}
	onion.configure(c.OnionHostname, c.OnionHostnameFile)
	return c
}

// onionSource resolves the .onion hostname for Onion-Location. A pinned
// hostname wins outright. Otherwise the file tor exports is read on demand:
// the first successful read is cached for the life of the process (the
// address is a function of the key, which does not change while tor runs),
// and while the file is absent or empty — tor is started after this process
// and writes it once it has derived the address — the file is re-tried at
// most once per onionRetryInterval so a request never pays more than one
// stat per interval.
type onionSource struct {
	mu        sync.Mutex
	pinned    string
	file      string
	resolved  string
	nextRetry time.Time
}

const onionRetryInterval = 30 * time.Second

var onion onionSource

func (o *onionSource) configure(pinned, file string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pinned = normalizeOnionHostname(pinned)
	if pinned != "" && o.pinned == "" {
		log.Printf("[config] ONION_HOSTNAME %q is not a .onion hostname — ignored; Onion-Location will use %s", pinned, file)
	}
	o.file = file
	o.resolved = ""
	o.nextRetry = time.Time{}
}

// OnionHost returns the .onion hostname to advertise, or "" when none is
// known yet. Safe for concurrent use from request handlers.
func OnionHost() string {
	return onion.host(time.Now())
}

func (o *onionSource) host(now time.Time) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pinned != "" {
		return o.pinned
	}
	if o.resolved != "" {
		return o.resolved
	}
	if o.file == "" || now.Before(o.nextRetry) {
		return ""
	}
	o.nextRetry = now.Add(onionRetryInterval)
	b, err := os.ReadFile(o.file)
	if err != nil {
		// Absent until tor has derived the address; anything else is worth a
		// line, but neither is a reason to fail the request.
		if !os.IsNotExist(err) {
			log.Printf("[config] onion hostname file %s: %v", o.file, err)
		}
		return ""
	}
	h := normalizeOnionHostname(string(b))
	if h == "" {
		if len(strings.TrimSpace(string(b))) > 0 {
			log.Printf("[config] onion hostname file %s does not hold a .onion hostname — ignored", o.file)
		}
		return ""
	}
	o.resolved = h
	log.Printf("[config] Onion-Location: %s (from %s)", h, o.file)
	return h
}

// normalizeOnionHostname trims what tor writes (the address and a newline),
// lower-cases it, and returns "" for anything that is not a bare .onion
// hostname — a header built from a value with a slash, a scheme or whitespace
// in it would send every visitor's browser somewhere other than the service.
func normalizeOnionHostname(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if !strings.HasSuffix(s, ".onion") || len(s) == len(".onion") {
		return ""
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-'
		if !ok {
			return ""
		}
	}
	return s
}

func getEnv(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func getIntEnv(k string, fallback int) int {
	if v := os.Getenv(k); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

func getBoolEnv(k string, fallback bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func getDurationEnv(k string, fallbackMs int) time.Duration {
	return time.Duration(getIntEnv(k, fallbackMs))
}

func sanitizeURL(url string) string {
	if url == "" {
		return "(not set)"
	}
	for i, c := range url {
		if c == '@' {
			return "postgres://***@" + url[i+1:]
		}
	}
	return url
}
