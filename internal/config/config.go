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

// DefaultHistorySyncTimeout bounds how long an on-demand backfill waits for the
// phone to answer. The request travels to the user's primary device, which may
// be asleep, so this is generous by design.
const DefaultHistorySyncTimeout = 30 * time.Second

// DefaultMediaDir is where downloaded media is written when MEDIA_DIR is unset.
const DefaultMediaDir = "./data/media"

// DefaultMediaMaxBytes refuses a single attachment above 100 MiB.
//
// The gateway shares a small server with other things and media is fetched on
// demand rather than in advance, so the limit is about one request, not about a
// quota: a single video must not be able to fill the disk.
const DefaultMediaMaxBytes = 100 << 20 // 100 MiB

// knownMediaTypes is every media type the gateway records a reference for.
//
// References are stored for all of them because a reference is a few hundred
// bytes; MEDIA_FETCH_TYPES decides which of them may be turned into a file.
// vcard and location name a type and carry no bytes, so they are not fetchable
// and are not listed here.
var knownMediaTypes = map[string]bool{
	"ptt":      true,
	"audio":    true,
	"image":    true,
	"video":    true,
	"document": true,
	"sticker":  true,
	"gif":      true,
}

// DefaultMediaFetchTypes returns the media types downloaded when
// MEDIA_FETCH_TYPES is unset.
//
// Stickers and gifs are deliberately out. They are the two types nobody asks an
// assistant to read back, and they are numerous enough that fetching them would
// be most of the disk for none of the value. Their references are still stored,
// so opting them in later costs one environment variable and no backfill.
func DefaultMediaFetchTypes() []string {
	// A fresh slice per call: the defaults are not a caller's to rewrite.
	return []string{"ptt", "audio", "image", "video", "document"}
}

// HistorySyncScope says which chat types a pushed history sync is stored in
// full for.
//
// It is a volume control, not a privacy switch: chats outside the scope still
// keep their most recent message, because a chat with no stored message can
// never be used as an anchor for an on-demand backfill later.
type HistorySyncScope string

const (
	// HistorySyncScopeDM stores direct conversations in full. It is the
	// default, because groups and channels are where the volume is.
	HistorySyncScopeDM HistorySyncScope = "dm"
	// HistorySyncScopeDMGroup adds group chats.
	HistorySyncScopeDMGroup HistorySyncScope = "dm,group"
	// HistorySyncScopeAll adds everything else, newsletters (channels)
	// included.
	HistorySyncScopeAll HistorySyncScope = "all"
)

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
	// HistorySyncScope decides which chat types a pushed history sync is
	// stored in full for.
	HistorySyncScope HistorySyncScope
	// HistorySyncTimeout bounds how long an on-demand backfill waits for the
	// phone to answer.
	HistorySyncTimeout time.Duration
	// MediaMaxBytes is the largest attachment the gateway will download.
	MediaMaxBytes int64
	// MediaFetchTypes are the media types an on-demand fetch may download. A
	// reference is stored for every type either way.
	MediaFetchTypes []string
}

// LoadGateway reads the gateway configuration from the environment.
func LoadGateway() (Gateway, error) {
	cfg := Gateway{
		AdminToken:        strings.TrimSpace(os.Getenv("ADMIN_TOKEN")),
		DBPath:            envOr("DB_PATH", "./data/whats-cloud.db"),
		MediaDir:          envOr("MEDIA_DIR", DefaultMediaDir),
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

	scope, err := ParseHistorySyncScope(os.Getenv("HISTORY_SYNC_SCOPE"))
	if err != nil {
		return Gateway{}, err
	}
	cfg.HistorySyncScope = scope

	timeout, err := parseHistorySyncTimeout(os.Getenv("HISTORY_SYNC_TIMEOUT"))
	if err != nil {
		return Gateway{}, err
	}
	cfg.HistorySyncTimeout = timeout

	maxBytes, err := ParseMediaMaxBytes(os.Getenv("MEDIA_MAX_BYTES"))
	if err != nil {
		return Gateway{}, err
	}
	cfg.MediaMaxBytes = maxBytes

	fetchTypes, err := ParseMediaFetchTypes(os.Getenv("MEDIA_FETCH_TYPES"))
	if err != nil {
		return Gateway{}, err
	}
	cfg.MediaFetchTypes = fetchTypes

	return cfg, nil
}

// ParseMediaMaxBytes reads MEDIA_MAX_BYTES as a plain byte count.
//
// A count rather than a "100MiB" string: the value guards a shared disk, and an
// unparsed unit suffix that silently became a byte count would be the worst way
// to discover the difference.
func ParseMediaMaxBytes(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultMediaMaxBytes, nil
	}
	maxBytes, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config: MEDIA_MAX_BYTES %q is not a byte count", raw)
	}
	if maxBytes <= 0 {
		return 0, fmt.Errorf("config: MEDIA_MAX_BYTES %q must be positive", raw)
	}
	return maxBytes, nil
}

// ParseMediaFetchTypes reads MEDIA_FETCH_TYPES.
//
// An unrecognised type is an error rather than a silent drop: the setting
// decides which attachments can ever be downloaded, and a typo would present
// itself as media that mysteriously never arrives.
func ParseMediaFetchTypes(raw string) ([]string, error) {
	parts := splitList(strings.ToLower(raw))
	if len(parts) == 0 {
		return DefaultMediaFetchTypes(), nil
	}
	for _, part := range parts {
		if !knownMediaTypes[part] {
			return nil, fmt.Errorf("config: MEDIA_FETCH_TYPES %q names an unknown media type %q", raw, part)
		}
	}
	return parts, nil
}

// ParseHistorySyncScope reads HISTORY_SYNC_SCOPE.
//
// The three accepted values are "dm" (the default), "dm,group" and "all". The
// parts may be reordered, spaced and cased freely, but anything outside those
// three sets is an error rather than a silent fallback: the setting decides how
// much of the user's history lands on disk, so a typo must not quietly change
// it.
func ParseHistorySyncScope(raw string) (HistorySyncScope, error) {
	parts := splitList(strings.ToLower(raw))
	if len(parts) == 0 {
		return HistorySyncScopeDM, nil
	}

	// splitList already de-duplicates, so a repeated part is the same scope.
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		switch part {
		case "dm", "group", "all":
			seen[part] = true
		default:
			return "", fmt.Errorf("config: HISTORY_SYNC_SCOPE %q is not one of dm, \"dm,group\" or all", raw)
		}
	}

	switch {
	case len(seen) == 1 && seen["dm"]:
		return HistorySyncScopeDM, nil
	case len(seen) == 2 && seen["dm"] && seen["group"]:
		return HistorySyncScopeDMGroup, nil
	case len(seen) == 1 && seen["all"]:
		return HistorySyncScopeAll, nil
	default:
		return "", fmt.Errorf("config: HISTORY_SYNC_SCOPE %q is not one of dm, \"dm,group\" or all", raw)
	}
}

// parseHistorySyncTimeout reads HISTORY_SYNC_TIMEOUT as a Go duration.
func parseHistorySyncTimeout(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultHistorySyncTimeout, nil
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("config: HISTORY_SYNC_TIMEOUT %q is not a duration", raw)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("config: HISTORY_SYNC_TIMEOUT %q must be positive", raw)
	}
	return timeout, nil
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
