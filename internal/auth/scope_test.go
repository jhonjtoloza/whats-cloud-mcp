package auth

import (
	"reflect"
	"testing"
)

func TestHasScope(t *testing.T) {
	tests := []struct {
		name     string
		granted  []string
		required string
		want     bool
	}{
		{"exact match", []string{"messages:send"}, "messages:send", true},
		{"match among several", []string{"messages:read", "messages:send"}, "messages:send", true},
		{"missing scope denies", []string{"messages:read"}, "messages:send", false},
		{"empty grant denies", nil, "messages:send", false},
		{"empty grant slice denies", []string{}, "messages:send", false},
		{"unrelated scope denies", []string{"sessions:read"}, "media:read", false},
		{"resource wildcard matches", []string{"media:*"}, "media:read", true},
		{"resource wildcard matches write too", []string{"media:*"}, "media:write", true},
		{"resource wildcard does not cross resources", []string{"media:*"}, "messages:send", false},
		{"global wildcard matches anything", []string{"*"}, "messages:send", true},
		{"global wildcard matches media", []string{"*"}, "media:write", true},
		{"action wildcard on required is never honoured", []string{"messages:send"}, "messages:*", false},
		{"empty required scope denies", []string{"*"}, "", false},
		{"blank granted entries are ignored", []string{"", "  ", "messages:send"}, "messages:send", true},
		{"whitespace is trimmed", []string{" messages:send "}, "messages:send", true},
		{"case sensitive", []string{"Messages:Send"}, "messages:send", false},
		{"partial prefix does not match", []string{"messages:sendx"}, "messages:send", false},
		{"wildcard alone in resource position is not a global wildcard", []string{"*:read"}, "media:read", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasScope(tc.granted, tc.required); got != tc.want {
				t.Errorf("HasScope(%q, %q) = %v, want %v", tc.granted, tc.required, got, tc.want)
			}
		})
	}
}

func TestParseScopes(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"single scope", "messages:send", []string{"messages:send"}},
		{"comma separated", "messages:send,messages:read", []string{"messages:send", "messages:read"}},
		{"spaces are trimmed", " messages:send , messages:read ", []string{"messages:send", "messages:read"}},
		{"empty string yields no scopes", "", nil},
		{"only separators yields no scopes", " , , ", nil},
		{"duplicates are removed", "messages:send,messages:send", []string{"messages:send"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseScopes(tc.raw)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseScopes(%q) = %#v, want %#v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestFormatScopes(t *testing.T) {
	tests := []struct {
		name   string
		scopes []string
		want   string
	}{
		{"single", []string{"messages:send"}, "messages:send"},
		{"several", []string{"messages:send", "media:*"}, "messages:send,media:*"},
		{"nil", nil, ""},
		{"blank entries dropped", []string{"", "messages:send"}, "messages:send"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatScopes(tc.scopes); got != tc.want {
				t.Errorf("FormatScopes(%#v) = %q, want %q", tc.scopes, got, tc.want)
			}
		})
	}
}

func TestValidateScopes(t *testing.T) {
	tests := []struct {
		name    string
		scopes  []string
		wantErr bool
	}{
		{"all known scopes", []string{ScopeMessagesSend, ScopeMessagesRead, ScopeMediaRead, ScopeMediaWrite, ScopeSessionsRead}, false},
		{"resource wildcard", []string{"media:*"}, false},
		{"global wildcard", []string{"*"}, false},
		{"unknown resource", []string{"billing:read"}, true},
		{"unknown action", []string{"messages:delete"}, true},
		{"malformed", []string{"messages"}, true},
		{"empty list is allowed", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateScopes(tc.scopes)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateScopes(%#v) error = %v, wantErr %v", tc.scopes, err, tc.wantErr)
			}
		})
	}
}
