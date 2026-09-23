package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestGenerateKeyFormat(t *testing.T) {
	tests := []struct {
		name       string
		tenantID   string
		wantPrefix string
	}{
		{
			name:       "uuid tenant id is shortened",
			tenantID:   "9f1c7a2e-5b4d-4a11-8c3f-2d6e7b0a1c45",
			wantPrefix: "wc_live_9f1c7a2e_",
		},
		{
			name:       "short tenant id is used verbatim",
			tenantID:   "acme",
			wantPrefix: "wc_live_acme_",
		},
		{
			name:       "non alphanumeric characters are stripped",
			tenantID:   "a-b-c-d-e-f-g-h-i",
			wantPrefix: "wc_live_abcdefgh_",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GenerateKey(tc.tenantID)
			if err != nil {
				t.Fatalf("GenerateKey() error = %v", err)
			}
			if !strings.HasPrefix(got.Plaintext, tc.wantPrefix) {
				t.Errorf("plaintext %q does not start with %q", got.Plaintext, tc.wantPrefix)
			}
			if !strings.HasPrefix(got.Plaintext, got.Prefix) {
				t.Errorf("prefix %q is not a prefix of plaintext %q", got.Prefix, got.Plaintext)
			}
			if len(got.Prefix) >= len(got.Plaintext) {
				t.Errorf("prefix %q must be shorter than the plaintext key", got.Prefix)
			}
			if strings.Contains(got.Prefix, got.Plaintext[len(got.Plaintext)-8:]) {
				t.Errorf("prefix must not leak the tail of the secret")
			}
		})
	}
}

func TestGenerateKeyRejectsEmptyTenant(t *testing.T) {
	if _, err := GenerateKey(""); err == nil {
		t.Fatal("GenerateKey(\"\") expected an error, got nil")
	}
	if _, err := GenerateKey("---"); err == nil {
		t.Fatal("GenerateKey(\"---\") expected an error for a tenant id with no alphanumeric characters")
	}
}

func TestGenerateKeyHashMatchesSHA256OfPlaintext(t *testing.T) {
	got, err := GenerateKey("tenant-1")
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}

	sum := sha256.Sum256([]byte(got.Plaintext))
	want := hex.EncodeToString(sum[:])
	if got.Hash != want {
		t.Errorf("Hash = %q, want %q", got.Hash, want)
	}
	if len(got.Hash) != 64 {
		t.Errorf("Hash length = %d, want 64 hex characters", len(got.Hash))
	}
}

func TestGenerateKeyIsUnique(t *testing.T) {
	const iterations = 200
	seen := make(map[string]struct{}, iterations)

	for i := 0; i < iterations; i++ {
		got, err := GenerateKey("tenant-1")
		if err != nil {
			t.Fatalf("GenerateKey() error = %v", err)
		}
		if _, dup := seen[got.Plaintext]; dup {
			t.Fatalf("GenerateKey() produced a duplicate key after %d iterations", i)
		}
		seen[got.Plaintext] = struct{}{}
	}
}

func TestGenerateKeySecretHasEnoughEntropy(t *testing.T) {
	got, err := GenerateKey("tenant-1")
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}

	parts := strings.Split(got.Plaintext, "_")
	if len(parts) != 4 {
		t.Fatalf("plaintext %q should have exactly 4 underscore-separated parts, got %d", got.Plaintext, len(parts))
	}
	// 32 random bytes in base62 need at least 41 characters (256 / log2(62)).
	if len(parts[3]) < 41 {
		t.Errorf("secret part length = %d, want at least 41 characters for 32 random bytes", len(parts[3]))
	}
}

func TestVerifyKey(t *testing.T) {
	generated, err := GenerateKey("tenant-1")
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}

	tests := []struct {
		name      string
		plaintext string
		hash      string
		want      bool
	}{
		{"matching key", generated.Plaintext, generated.Hash, true},
		{"wrong plaintext", generated.Plaintext + "x", generated.Hash, false},
		{"empty plaintext", "", generated.Hash, false},
		{"empty hash", generated.Plaintext, "", false},
		{"both empty", "", "", false},
		{"uppercase hash still matches", generated.Plaintext, strings.ToUpper(generated.Hash), true},
		{"truncated hash", generated.Plaintext, generated.Hash[:32], false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifyKey(tc.plaintext, tc.hash); got != tc.want {
				t.Errorf("VerifyKey() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHashKeyIsStable(t *testing.T) {
	const plaintext = "wc_live_tenant1_abc"
	if HashKey(plaintext) != HashKey(plaintext) {
		t.Error("HashKey() is not deterministic")
	}
	if HashKey(plaintext) == HashKey(plaintext+"x") {
		t.Error("HashKey() collided for different inputs")
	}
}

func TestVerifyAdminToken(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		presented  string
		want       bool
	}{
		{"matching token", "s3cret", "s3cret", true},
		{"wrong token", "s3cret", "nope", false},
		{"empty configured token always denies", "", "", false},
		{"empty configured token denies any presented token", "", "anything", false},
		{"empty presented token denies", "s3cret", "", false},
		{"prefix of the token denies", "s3cret", "s3c", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifyAdminToken(tc.configured, tc.presented); got != tc.want {
				t.Errorf("VerifyAdminToken() = %v, want %v", got, tc.want)
			}
		})
	}
}
