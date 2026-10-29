// Package config loads the twelve-factor configuration from environment
// variables (Railway injects PORT and DATABASE_URL).
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Role string

const (
	RoleAll    Role = "all"
	RoleAPI    Role = "api"
	RoleWorker Role = "worker"
)

// APIKey is a hashed API key with a non-secret identifier.
type APIKey struct {
	ID   string
	Hash [32]byte
}

type Config struct {
	Env         string
	Role        Role
	Port        string
	DatabaseURL string
	DBMaxConns  int32
	LogLevel    string
	LogFormat   string

	APIKeys        []APIKey
	AuthDisabled   bool
	RatePerKey     float64
	CORSOrigins    []string
	MaxResultsCap  int
	MaxBodyBytes   int64
	WebhookSecret  string
	PprofAddr      string
	ShutdownWindow time.Duration

	// sources
	PlacesAPIKey      string
	PlacesFieldTier   string
	EnableGMapsHTTP   bool
	GMapsProxies      []string
	GMapsMaxPages     int
	GMapsScraperURL   string
	GMapsScraperEmail bool
	EnableOSM         bool
	OverpassURLs      []string
	EnableSirene      bool
	SireneURL         string
	EnableMock        bool

	Geocoder        string
	NominatimURL    string
	GeocoderUA      string
	EnrichEnabled   bool
	EnrichConc      int
	EnrichMaxPages  int
	SourceConc      map[string]int
	SourceRate      map[string]float64
	MaxCellsPerJob  int
	DedupCacheSize  int
	PlanConcurrency int
	Lease           time.Duration
}

func Load() (*Config, error) {
	c := &Config{
		Env:            env("APP_ENV", "production"),
		Role:           Role(strings.ToLower(env("ROLE", "all"))),
		Port:           env("PORT", "8080"),
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		DBMaxConns:     int32(envInt("DB_MAX_CONNS", 20)),
		LogLevel:       env("LOG_LEVEL", "info"),
		LogFormat:      env("LOG_FORMAT", "json"),
		AuthDisabled:   envBool("AUTH_DISABLED", false),
		RatePerKey:     envFloat("API_RATE_PER_KEY", 20),
		CORSOrigins:    envList("CORS_ORIGINS"),
		MaxResultsCap:  envInt("MAX_RESULTS_CAP", 50_000),
		MaxBodyBytes:   int64(envInt("MAX_BODY_BYTES", 1<<20)),
		WebhookSecret:  os.Getenv("WEBHOOK_SECRET"),
		PprofAddr:      os.Getenv("PPROF_ADDR"),
		ShutdownWindow: envDuration("SHUTDOWN_WINDOW", 25*time.Second),

		PlacesAPIKey:      os.Getenv("GOOGLE_PLACES_API_KEY"),
		PlacesFieldTier:   env("PLACES_FIELD_TIER", "contact"),
		EnableGMapsHTTP:   envBool("ENABLE_GMAPS_SCRAPING", false),
		GMapsProxies:      envList("GMAPS_PROXIES"),
		GMapsMaxPages:     envInt("GMAPS_MAX_PAGES", 3),
		GMapsScraperURL:   os.Getenv("GMAPS_SCRAPER_URL"),
		GMapsScraperEmail: envBool("GMAPS_SCRAPER_EMAIL", false),
		EnableOSM:         envBool("ENABLE_OSM", true),
		OverpassURLs:      envList("OVERPASS_URL"),
		EnableSirene:      envBool("ENABLE_SIRENE", true),
		SireneURL:         os.Getenv("SIRENE_URL"),
		EnableMock:        envBool("ENABLE_MOCK_SOURCE", false),

		Geocoder:        env("GEOCODER", ""),
		NominatimURL:    os.Getenv("NOMINATIM_URL"),
		GeocoderUA:      env("GEOCODER_USER_AGENT", "CLEADS-Business-Extractor/1.0"),
		EnrichEnabled:   envBool("ENRICH_ENABLED", true),
		EnrichConc:      envInt("ENRICH_CONCURRENCY", 32),
		EnrichMaxPages:  envInt("ENRICH_MAX_PAGES", 4),
		MaxCellsPerJob:  envInt("MAX_CELLS_PER_JOB", 2500),
		DedupCacheSize:  envInt("DEDUP_CACHE_SIZE", 2_000_000),
		PlanConcurrency: envInt("PLAN_CONCURRENCY", 4),
		Lease:           envDuration("TASK_LEASE", 2*time.Minute),
	}
	var err error
	if c.SourceConc, err = parseIntMap(os.Getenv("SOURCE_CONCURRENCY")); err != nil {
		return nil, fmt.Errorf("SOURCE_CONCURRENCY: %w", err)
	}
	if c.SourceRate, err = parseFloatMap(os.Getenv("SOURCE_RATE")); err != nil {
		return nil, fmt.Errorf("SOURCE_RATE: %w", err)
	}
	for i, raw := range envList("API_KEYS") {
		id, key, ok := strings.Cut(raw, ":")
		if !ok {
			id, key = "key"+strconv.Itoa(i+1), raw
		}
		if len(key) < 24 {
			return nil, fmt.Errorf("API_KEYS: key %q is too short (min 24 chars)", id)
		}
		c.APIKeys = append(c.APIKeys, APIKey{ID: id, Hash: sha256.Sum256([]byte(key))})
	}
	if c.Geocoder == "" {
		c.Geocoder = "nominatim"
		if c.PlacesAPIKey != "" {
			c.Geocoder = "google"
		}
	}
	return c, c.validate()
}

func (c *Config) validate() error {
	var errs []error
	switch c.Role {
	case RoleAll, RoleAPI, RoleWorker:
	default:
		errs = append(errs, fmt.Errorf("ROLE must be all, api or worker (got %q)", c.Role))
	}
	if c.DatabaseURL == "" && c.Role != RoleAll {
		errs = append(errs, errors.New("DATABASE_URL is required when ROLE is api or worker"))
	}
	if c.Role != RoleWorker && len(c.APIKeys) == 0 && !c.AuthDisabled {
		errs = append(errs, errors.New("API_KEYS is required (or set AUTH_DISABLED=true for local development)"))
	}
	if c.AuthDisabled && c.Env == "production" {
		errs = append(errs, errors.New("AUTH_DISABLED=true is refused when APP_ENV=production"))
	}
	if c.Geocoder != "google" && c.Geocoder != "nominatim" && c.Geocoder != "none" {
		errs = append(errs, fmt.Errorf("GEOCODER must be google, nominatim or none"))
	}
	if c.Geocoder == "google" && c.PlacesAPIKey == "" {
		errs = append(errs, errors.New("GEOCODER=google requires GOOGLE_PLACES_API_KEY"))
	}
	return errors.Join(errs...)
}

// KeyHash hashes a presented API key.
func KeyHash(k string) [32]byte { return sha256.Sum256([]byte(k)) }

// Redacted returns a log-safe summary.
func (c *Config) Redacted() map[string]any {
	ids := make([]string, len(c.APIKeys))
	for i, k := range c.APIKeys {
		ids[i] = k.ID
	}
	h := sha256.Sum256([]byte(c.DatabaseURL))
	return map[string]any{
		"env": c.Env, "role": c.Role, "port": c.Port, "database": c.DatabaseURL != "",
		"database_fingerprint": hex.EncodeToString(h[:4]), "api_keys": ids,
		"places_api": c.PlacesAPIKey != "", "gmaps_http": c.EnableGMapsHTTP, "gmaps_browser": c.GMapsScraperURL != "",
		"osm": c.EnableOSM, "sirene": c.EnableSirene, "mock": c.EnableMock, "geocoder": c.Geocoder, "enrich": c.EnrichEnabled,
	}
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	if v, err := strconv.ParseBool(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envList(k string) []string {
	var out []string
	for _, p := range strings.Split(os.Getenv(k), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseIntMap(s string) (map[string]int, error) {
	out := map[string]int{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		k, v, ok := strings.Cut(p, "=")
		n, err := strconv.Atoi(v)
		if !ok || err != nil {
			return nil, fmt.Errorf("bad entry %q (want name=int)", p)
		}
		out[strings.TrimSpace(k)] = n
	}
	return out, nil
}

func parseFloatMap(s string) (map[string]float64, error) {
	out := map[string]float64{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		k, v, ok := strings.Cut(p, "=")
		n, err := strconv.ParseFloat(v, 64)
		if !ok || err != nil {
			return nil, fmt.Errorf("bad entry %q (want name=float)", p)
		}
		out[strings.TrimSpace(k)] = n
	}
	return out, nil
}
