package config_test

import (
	"log/slog"
	"reflect"
	"testing"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/config"
)

// clearBindEnv removes every binding-related variable so a test starts from a
// known state regardless of what the developer has exported.
func clearBindEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BIND_ADDR", "")
	t.Setenv("PORT", "")
	t.Setenv("MCP_ALLOWED_ORIGINS", "")
}

func TestLoadGatewayDefaults(t *testing.T) {
	clearBindEnv(t)
	t.Setenv("ADMIN_TOKEN", "a-token")

	got, err := config.LoadGateway()
	if err != nil {
		t.Fatalf("LoadGateway() error = %v", err)
	}

	// The MCP spec says to bind loopback rather than every interface; a wider
	// bind must be an explicit operator decision.
	if got.BindAddr != "127.0.0.1:8080" {
		t.Errorf("BindAddr = %q, want 127.0.0.1:8080 (loopback by default)", got.BindAddr)
	}
	if got.DBPath != "./data/whats-cloud.db" {
		t.Errorf("DBPath = %q, want the default path", got.DBPath)
	}
	if got.MediaDir != "./data/media" {
		t.Errorf("MediaDir = %q, want the default path", got.MediaDir)
	}
	if got.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", got.LogLevel)
	}
	if len(got.MCPAllowedOrigins) != 0 {
		t.Errorf("MCPAllowedOrigins = %#v, want empty by default", got.MCPAllowedOrigins)
	}
}

func TestLoadGatewayRequiresAdminToken(t *testing.T) {
	clearBindEnv(t)
	t.Setenv("ADMIN_TOKEN", "")

	if _, err := config.LoadGateway(); err == nil {
		t.Fatal("LoadGateway() without ADMIN_TOKEN should fail rather than start an open admin surface")
	}
}

func TestLoadGatewayBindAddress(t *testing.T) {
	tests := []struct {
		name     string
		bindAddr string
		port     string
		want     string
		wantErr  bool
	}{
		{"defaults to loopback", "", "", "127.0.0.1:8080", false},
		{"PORT alone keeps loopback", "", "9999", "127.0.0.1:9999", false},
		{"BIND_ADDR is used verbatim", "0.0.0.0:8080", "", "0.0.0.0:8080", false},
		{"BIND_ADDR wins over PORT", "0.0.0.0:7000", "9999", "0.0.0.0:7000", false},
		{"host only is rejected", "0.0.0.0", "", "", true},
		{"non numeric port is rejected", "127.0.0.1:http", "", "", true},
		{"port out of range is rejected", "127.0.0.1:70000", "", "", true},
		{"bare PORT out of range is rejected", "", "70000", "", true},
		{"bare PORT non numeric is rejected", "", "abc", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearBindEnv(t)
			t.Setenv("ADMIN_TOKEN", "a-token")
			t.Setenv("BIND_ADDR", tc.bindAddr)
			t.Setenv("PORT", tc.port)

			got, err := config.LoadGateway()
			if (err != nil) != tc.wantErr {
				t.Fatalf("LoadGateway() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got.BindAddr != tc.want {
				t.Errorf("BindAddr = %q, want %q", got.BindAddr, tc.want)
			}
		})
	}
}

func TestLoadGatewayPortIsDerivedFromBindAddr(t *testing.T) {
	clearBindEnv(t)
	t.Setenv("ADMIN_TOKEN", "a-token")
	t.Setenv("BIND_ADDR", "0.0.0.0:7777")

	got, err := config.LoadGateway()
	if err != nil {
		t.Fatalf("LoadGateway() error = %v", err)
	}
	// The container healthcheck probes the local port, so it must be known.
	if got.Port != 7777 {
		t.Errorf("Port = %d, want 7777 derived from BIND_ADDR", got.Port)
	}
}

func TestLoadGatewayReadsEnvironment(t *testing.T) {
	clearBindEnv(t)
	t.Setenv("ADMIN_TOKEN", "a-token")
	t.Setenv("DB_PATH", "/tmp/custom.db")
	t.Setenv("MEDIA_DIR", "/tmp/media")
	t.Setenv("LOG_LEVEL", "debug")

	got, err := config.LoadGateway()
	if err != nil {
		t.Fatalf("LoadGateway() error = %v", err)
	}

	if got.DBPath != "/tmp/custom.db" {
		t.Errorf("DBPath = %q", got.DBPath)
	}
	if got.MediaDir != "/tmp/media" {
		t.Errorf("MediaDir = %q", got.MediaDir)
	}
	if got.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug", got.LogLevel)
	}
}

func TestLoadGatewayMCPAllowedOrigins(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"single origin", "https://claude.ai", []string{"https://claude.ai"}},
		{"several origins", "https://claude.ai,https://app.example.com", []string{"https://claude.ai", "https://app.example.com"}},
		{"whitespace is trimmed", " https://claude.ai , https://b.example.com ", []string{"https://claude.ai", "https://b.example.com"}},
		{"empty means none configured", "", nil},
		{"only separators means none configured", " , , ", nil},
		{"duplicates are removed", "https://claude.ai,https://claude.ai", []string{"https://claude.ai"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearBindEnv(t)
			t.Setenv("ADMIN_TOKEN", "a-token")
			t.Setenv("MCP_ALLOWED_ORIGINS", tc.raw)

			got, err := config.LoadGateway()
			if err != nil {
				t.Fatalf("LoadGateway() error = %v", err)
			}
			if !reflect.DeepEqual(got.MCPAllowedOrigins, tc.want) {
				t.Errorf("MCPAllowedOrigins = %#v, want %#v", got.MCPAllowedOrigins, tc.want)
			}
		})
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  slog.Level
	}{
		{"debug", "debug", slog.LevelDebug},
		{"info", "info", slog.LevelInfo},
		{"warn", "warn", slog.LevelWarn},
		{"warning alias", "warning", slog.LevelWarn},
		{"error", "error", slog.LevelError},
		{"uppercase", "DEBUG", slog.LevelDebug},
		{"empty falls back to info", "", slog.LevelInfo},
		{"unknown falls back to info", "loud", slog.LevelInfo},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := config.ParseLogLevel(tc.value); got != tc.want {
				t.Errorf("ParseLogLevel(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
