package config_test

import (
	"strings"
	"testing"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/config"
)

// TestParseMediaMaxBytes pins the shared-server guard. The default has to be
// generous enough for a real video and small enough that one of them cannot
// fill the box.
func TestParseMediaMaxBytes(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int64
		wantErr bool
	}{
		{name: "unset falls back to the default", raw: "", want: config.DefaultMediaMaxBytes},
		{name: "blank falls back to the default", raw: "   ", want: config.DefaultMediaMaxBytes},
		{name: "a plain byte count", raw: "5242880", want: 5242880},
		{name: "zero is not a size", raw: "0", wantErr: true},
		{name: "negative is not a size", raw: "-1", wantErr: true},
		{name: "prose is not a size", raw: "100MiB", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := config.ParseMediaMaxBytes(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseMediaMaxBytes(%q) = %d, want an error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMediaMaxBytes(%q) error = %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("ParseMediaMaxBytes(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

// TestDefaultMediaMaxBytesIs100MiB states the number the README promises.
func TestDefaultMediaMaxBytesIs100MiB(t *testing.T) {
	if config.DefaultMediaMaxBytes != 100<<20 {
		t.Errorf("DefaultMediaMaxBytes = %d, want 100 MiB", config.DefaultMediaMaxBytes)
	}
}

// TestParseMediaFetchTypes covers the allowlist that decides which references
// may become bytes on disk.
func TestParseMediaFetchTypes(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{
			name: "unset is the default set",
			raw:  "",
			want: []string{"ptt", "audio", "image", "video", "document"},
		},
		{
			name: "an explicit narrower set",
			raw:  "ptt,audio",
			want: []string{"ptt", "audio"},
		},
		{
			name: "spacing and casing are forgiven",
			raw:  " PTT , Image ",
			want: []string{"ptt", "image"},
		},
		{
			name: "repeats collapse",
			raw:  "image,image",
			want: []string{"image"},
		},
		{
			name: "stickers may be opted into",
			raw:  "image,sticker",
			want: []string{"image", "sticker"},
		},
		{
			// A typo must not silently produce an allowlist that downloads
			// nothing, which would look like broken media rather than a broken
			// setting.
			name:    "an unknown type is an error rather than a silent drop",
			raw:     "image,phot",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := config.ParseMediaFetchTypes(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseMediaFetchTypes(%q) = %v, want an error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMediaFetchTypes(%q) error = %v", tc.raw, err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("ParseMediaFetchTypes(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestDefaultMediaFetchTypesExcludeStickersAndGifs states the deliberate
// omission: references are kept for them, bytes are not.
func TestDefaultMediaFetchTypesExcludeStickersAndGifs(t *testing.T) {
	defaults := strings.Join(config.DefaultMediaFetchTypes(), ",")
	for _, excluded := range []string{"sticker", "gif"} {
		if strings.Contains(defaults, excluded) {
			t.Errorf("default media fetch types %q should not include %q", defaults, excluded)
		}
	}
}

// TestDefaultMediaFetchTypesAreIndependent guards against a caller mutating the
// package's own default slice.
func TestDefaultMediaFetchTypesAreIndependent(t *testing.T) {
	first := config.DefaultMediaFetchTypes()
	first[0] = "tampered"

	if second := config.DefaultMediaFetchTypes(); second[0] == "tampered" {
		t.Error("DefaultMediaFetchTypes returns a shared slice; a caller can rewrite the defaults")
	}
}
