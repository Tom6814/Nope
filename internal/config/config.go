package config

import (
	"flag"
	"os"
	"strings"
)

type Config struct {
	ListenAddr string
	DBPath     string
	RPOrigin   string // WebAuthn Relying Party origin, e.g. "http://localhost:9800"
	RPID       string // WebAuthn Relying Party ID, e.g. "localhost"
	RPName     string
	Secret     string // server secret for token encryption

	// Storage (MinIO / S3, or local filesystem)
	StorageEndpoint  string
	StorageAccessKey string
	StorageSecretKey string
	StorageBucket    string
	StorageSSL       bool
	StoragePublicURL string
	StoragePath      string // local filesystem path (used when S3 is not configured)

	// OAuth providers
	GitHubClientID     string
	GitHubClientSecret string
	LinuxDoClientID     string
	LinuxDoClientSecret string
}

func Parse() *Config {
	cfg := &Config{}
	flag.StringVar(&cfg.ListenAddr, "listen", defaultListen(), "listen address")
	flag.StringVar(&cfg.DBPath, "db", envOr("DATABASE_URL", DefaultDBPath()), "database path or PostgreSQL URL")
	flag.StringVar(&cfg.RPOrigin, "origin", envOr("RP_ORIGIN", "http://localhost:9800"), "WebAuthn RP origin")
	flag.StringVar(&cfg.RPID, "rpid", envOr("RP_ID", "localhost"), "WebAuthn RP ID")
	flag.StringVar(&cfg.RPName, "rpname", envOr("RP_NAME", "OpeniLink Hub"), "WebAuthn RP display name")
	flag.StringVar(&cfg.Secret, "secret", envOr("SECRET", "change-me-in-production"), "server secret")
	// Storage
	cfg.StorageEndpoint = envOr("STORAGE_ENDPOINT", "")
	cfg.StorageAccessKey = envOr("STORAGE_ACCESS_KEY", "")
	cfg.StorageSecretKey = envOr("STORAGE_SECRET_KEY", "")
	cfg.StorageBucket = envOr("STORAGE_BUCKET", "openilink")
	cfg.StorageSSL = envOr("STORAGE_SSL", "") == "true"
	cfg.StoragePublicURL = envOr("STORAGE_PUBLIC_URL", "")
	cfg.StoragePath = envOr("STORAGE_PATH", "")
	// OAuth
	cfg.GitHubClientID = envOr("GITHUB_CLIENT_ID", "")
	cfg.GitHubClientSecret = envOr("GITHUB_CLIENT_SECRET", "")
	cfg.LinuxDoClientID = envOr("LINUXDO_CLIENT_ID", "")
	cfg.LinuxDoClientSecret = envOr("LINUXDO_CLIENT_SECRET", "")
	flag.Parse()
	return cfg
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// defaultListen resolves the listen address: $PORT first (PaaS convention —
// Zeabur/Railway inject PORT as a bare port number), then $LISTEN, then ":9800".
func defaultListen() string {
	if v := os.Getenv("PORT"); v != "" {
		if strings.HasPrefix(v, ":") {
			return v
		}
		return ":" + v
	}
	if v := os.Getenv("LISTEN"); v != "" {
		return v
	}
	return ":9800"
}
