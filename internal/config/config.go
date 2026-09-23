// Package config reads process configuration from the environment. Nothing
// here reaches the filesystem or the network, so it stays trivially testable.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// defaultBindAddr binds the loopback interface only.
//
// The MCP Streamable HTTP spec is explicit that a local server SHOULD bind
// 127.0.0.1 rather than 0.0.0.0, so that a machine on the same network cannot
// reach it. Widening the bind is an operator decision made through BIND_ADDR.
const defaultBindAddr = "127.0.0.1:8080"

// Gateway is the configuration of the HTTP daemon.
type Gateway struct {
	// AdminToken guards the admin credential class. Required.
	AdminToken string
	// DBPath is the SQLite file shared by our schema and whatsmeow's.
	DBPath string
	// MediaDir is where downloaded media is written.
	MediaDir string
	// BindAddr is the host:port the HTTP server listens on.
	BindAddr string
	// Port is the port half of BindAddr, kept so the container healthcheck can
	// probe the local endpoint.
	Port int
	// MCPAllowedOrigins is the allowlist checked against the Origin header on
	// /mcp requests. Empty means no browser origin is accepted.
	MCPAllowedOrigins []string
	// LogLevel is the minimum level for structured logging.
	LogLevel slog.Level
	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout time.Duration
}

// LoadGateway reads the gateway configuration from the environment.
func LoadGateway() (Gateway, error) {
	cfg := Gateway{
		AdminToken:        strings.TrimSpace(os.Getenv("ADMIN_TOKEN")),
		DBPath:            envOr("DB_PATH", "./data/whats-cloud.db"),
		MediaDir:          envOr("MEDIA_DIR", "./data/media"),
		MCPAllowedOrigins: splitList(os.Getenv("MCP_ALLOWED_ORIGINS")),
		LogLevel:          ParseLogLevel(os.Getenv("LOG_LEVEL")),
		ShutdownTimeout:   15 * time.Second,
	}

	if cfg.AdminToken == "" {
		return Gateway{}, errors.New("config: ADMIN_TOKEN is required")
	}

	bindAddr, port, err := resolveBindAddr(
		strings.TrimSpace(os.Getenv("BIND_ADDR")),
		strings.TrimSpace(os.Getenv("PORT")),
	)
	if err != nil {
		return Gateway{}, err
	}
	cfg.BindAddr = bindAddr
	cfg.Port = port

	return cfg, nil
}

// resolveBindAddr decides where to listen.
//
// BIND_ADDR wins when set. Otherwise PORT only changes the port, leaving the
// host on loopback, so raising the port never silently exposes the gateway to
// the whole network.
func resolveBindAddr(bindAddr, port string) (string, int, error) {
	if bindAddr == "" {
		if port == "" {
			bindAddr = defaultBindAddr
		} else {
			parsed, err := parsePort(port)
			if err != nil {
				return "", 0, err
			}
			bindAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(parsed))
		}
	}

	host, rawPort, err := net.SplitHostPort(bindAddr)
	if err != nil {
		return "", 0, fmt.Errorf("config: BIND_ADDR %q must be host:port", bindAddr)
	}
	parsed, err := parsePort(rawPort)
	if err != nil {
		return "", 0, err
	}

	return net.JoinHostPort(host, strconv.Itoa(parsed)), parsed, nil
}

// ParseLogLevel maps a level name onto slog. Anything unrecognised becomes info
// rather than failing startup over a log setting.
func ParseLogLevel(value string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// splitList parses a comma-separated env var into a de-duplicated slice.
func splitList(raw string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	return out
}

func parsePort(raw string) (int, error) {
	port, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: port %q is not a number", raw)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("config: port %d is out of range", port)
	}
	return port, nil
}
