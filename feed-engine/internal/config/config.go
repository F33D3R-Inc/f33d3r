package config

import (
	"log"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port         string
	Host         string
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// Database
	DatabaseURL string
	// AethyrRank
	AethyrRankURL     string
	AethyrRankTimeout time.Duration
	// Zior
	ZiorURL string
	// Ain-Soph wallet
	AinSophURL string
	// Vovin (aethyr-msg) E2E messaging
	VovinURL string
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
	// Thessalon (commerce brain — subscriptions, PPV, tips)
	ThessalonURL string
	// Transcoding brain — ffmpeg video processing (separate server in production)
	TranscodingURL string
	// eKYC brain — face match, OCR, anti-spoof (Python, port 8099)
	EKYCUrl string
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
	// Dev
	DevMode    bool
	ShowScores bool
}

func Load() *Config {
	c := &Config{
		Port:              getEnv("PORT", "8081"),
		Host:              getEnv("HOST", "0.0.0.0"),
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		DatabaseURL:       getEnv("DATABASE_URL", ""),
		AethyrRankURL:     getEnv("AETHYRRANK_URL", "http://localhost:8080"),
		AethyrRankTimeout: getDurationEnv("AETHYRRANK_TIMEOUT_MS", 200) * time.Millisecond,
		ZiorURL:           getEnv("ZIOR_URL", "http://localhost:8082"),
		AinSophURL:        getEnv("AIN_SOPH_URL", "http://localhost:8089"),
		VovinURL:          getEnv("VOVIN_URL", "http://localhost:8092"),
		ZodacareURL:       getEnv("ZODACARE_URL", "http://localhost:8090"),
		ElohimVeniURL:     getEnv("ELOHIM_VENI_URL", "http://localhost:8093"),
		VerityURL:         getEnv("VERITY_URL", "http://localhost:8095"),
		LedgerURL:         getEnv("LEDGER_URL", "http://localhost:8096"),
		CaeorURL:          getEnv("CAEOR_URL", "http://localhost:8086"),
		ThessalonURL:      getEnv("THESSALON_URL", "http://localhost:8084"),
		TranscodingURL:    getEnv("TRANSCODING_URL", "http://transcoding:8098"),
		EKYCUrl:           getEnv("EKYC_URL", "http://ekyc:8099"),
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
		DevMode:           getBoolEnv("DEV_MODE", false),
		ShowScores:        getBoolEnv("SHOW_SCORES", false),
	}
	log.Printf("[config] AethyrRank: %s | Zior: %s | DB: %s",
		c.AethyrRankURL, c.ZiorURL, sanitizeURL(c.DatabaseURL))
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
