package wa

import (
	"errors"
	"testing"
)

// TestParseJID pins the normalisation contract for recipient addresses.
//
// The asymmetry this covers is deliberate. A bare number is user input and may
// arrive from a web form carrying "+", spaces or dashes, so it is normalised to
// digits. A full JID is an address, and its user part may legitimately contain
// characters that are not digits: group ids embed a dash. Stripping those would
// corrupt a valid address, so anything containing "@" is passed through
// untouched.
func TestParseJID(t *testing.T) {
	const userServer = "s.whatsapp.net"

	tests := []struct {
		name       string
		raw        string
		wantUser   string
		wantServer string
		wantErr    bool
	}{
		{
			name:       "bare number gains the default user server",
			raw:        "573001234567",
			wantUser:   "573001234567",
			wantServer: userServer,
		},
		{
			name:       "bare number keeps only its digits",
			raw:        "+57 300 123-4567",
			wantUser:   "573001234567",
			wantServer: userServer,
		},
		{
			name:       "surrounding whitespace is ignored",
			raw:        "  573001234567  ",
			wantUser:   "573001234567",
			wantServer: userServer,
		},
		{
			name:       "full user JID is passed through",
			raw:        "573001234567@s.whatsapp.net",
			wantUser:   "573001234567",
			wantServer: userServer,
		},
		{
			name:       "group JID keeps the dash in its user part",
			raw:        "123456789-987654@g.us",
			wantUser:   "123456789-987654",
			wantServer: "g.us",
		},
		{
			name:       "linked-device JID is passed through",
			raw:        "573001234567@lid",
			wantUser:   "573001234567",
			wantServer: "lid",
		},
		{
			name:    "empty input is rejected",
			raw:     "",
			wantErr: true,
		},
		{
			name:    "whitespace only is rejected",
			raw:     "   ",
			wantErr: true,
		},
		{
			name:    "a bare string with no digits is rejected",
			raw:     "not-a-number",
			wantErr: true,
		},
		{
			name:    "punctuation that strips to nothing is rejected",
			raw:     "+++",
			wantErr: true,
		},
		{
			name:    "a JID with no user part is rejected",
			raw:     "@s.whatsapp.net",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			jid, err := parseJID(tc.raw)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseJID(%q) = %v, want an error", tc.raw, jid)
				}
				if !errors.Is(err, ErrInvalidJID) {
					t.Fatalf("parseJID(%q) error = %v, want ErrInvalidJID", tc.raw, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("parseJID(%q) returned unexpected error: %v", tc.raw, err)
			}
			if jid.User != tc.wantUser {
				t.Errorf("parseJID(%q).User = %q, want %q", tc.raw, jid.User, tc.wantUser)
			}
			if jid.Server != tc.wantServer {
				t.Errorf("parseJID(%q).Server = %q, want %q", tc.raw, jid.Server, tc.wantServer)
			}
		})
	}
}
