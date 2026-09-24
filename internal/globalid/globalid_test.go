package globalid

import (
	"strings"
	"testing"
)

const testULID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

func TestParse_ValidTokens(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  Token
	}{
		{"host and port", testULID + "@203.0.113.9:5555", Token{ULID: testULID, HostPort: "203.0.113.9:5555"}},
		{"host without port fills default", testULID + "@example.com", Token{ULID: testULID, HostPort: "example.com:5555"}},
		{"lowercase ulid is normalized", strings.ToLower(testULID) + "@example.com:1234", Token{ULID: testULID, HostPort: "example.com:1234"}},
		{"bracketed ipv6 with port", testULID + "@[::1]:5555", Token{ULID: testULID, HostPort: "[::1]:5555"}},
		{"bracketed ipv6 without port fills default", testULID + "@[::1]", Token{ULID: testULID, HostPort: "[::1]:5555"}},
		{"surrounding whitespace is trimmed", "  " + testULID + "@example.com:5555\n", Token{ULID: testULID, HostPort: "example.com:5555"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Parse(c.input)
			if !ok {
				t.Fatalf("Parse(%q): ok=false, want true", c.input)
			}
			if got != c.want {
				t.Errorf("Parse(%q) = %+v, want %+v", c.input, got, c.want)
			}
		})
	}
}

// TestParse_RejectsBareULIDs is the disambiguation guarantee the CLI relies
// on: a bare local ULID (no '@') must never be misread as a global token.
func TestParse_RejectsBareULIDs(t *testing.T) {
	if _, ok := Parse(testULID); ok {
		t.Fatal("Parse accepted a bare ULID with no '@' - disambiguation is broken")
	}
}

func TestParse_RejectsMalformed(t *testing.T) {
	bad := []string{
		"",
		"@example.com:5555",           // empty ULID
		testULID + "@",                // empty host
		"not-a-ulid@example.com",      // invalid ULID
		testULID + "@::1",             // bare unbracketed IPv6, ambiguous without brackets
		testULID + "@example.com:abc", // non-numeric port
	}
	for _, s := range bad {
		if _, ok := Parse(s); ok {
			t.Errorf("Parse(%q): ok=true, want false", s)
		}
	}
}

func TestToken_StringAndFileSafe(t *testing.T) {
	tok := Token{ULID: testULID, HostPort: "example.com:5555"}
	if got, want := tok.String(), testULID+"@example.com:5555"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := tok.FileSafe(), testULID+"@example.com%3A5555"; got != want {
		t.Errorf("FileSafe() = %q, want %q", got, want)
	}
}
