package config

import (
	"log"
	"os"
	"strconv"
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
	// eKYC brain — face match, OCR, anti-spoof (Python, port 8099)
	EKYCUrl string
	// Alexandria — hypermedia library brain (content catalog)
	AlexandriaURL string
	// Herald push notification brain
	HeraldURL string
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
		EKYCUrl:           getEnv("EKYC_URL", "http://ekyc:8099"),
		AlexandriaURL:     getEnv("ALEXANDRIA_URL", "http://localhost:8098"),
		HeraldURL:         getEnv("HERALD_URL", ""),
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
		KlipyAPIKey:       getEnv("KLIPHY_API", ""),
		MediaTokenSecret:  getEnv("MEDIA_TOKEN_SECRET", ""),
		TiingoAPIKey:        getEnv("TIINGO_API_KEY", ""),
		TranslationAPIURL:  getEnv("TRANSLATION_API_URL", ""),
		TranslationAPIKey:  getEnv("TRANSLATION_API_KEY", ""),
		SecurityAlertEmail: getEnv("SECURITY_ALERT_EMAIL", ""),
		KafkaBrokers:       getEnv("KAFKA_BROKERS", ""),
		DevMode:            getBoolEnv("DEV_MODE", false),
		ShowScores:         getBoolEnv("SHOW_SCORES", false),
		TemplateStrict:     getBoolEnv("TEMPLATE_STRICT", false),
	}
	log.Printf("[config] AethyrRank: %s | Themis: %s | DB: %s",
		c.AethyrRankURL, c.ThemisURL, sanitizeURL(c.DatabaseURL))
	return c
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
