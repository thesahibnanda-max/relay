package naming

import (
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"Brave Fox":             "brave-fox",
		"  Ünï--cödé!! ":        "n-c-d",
		"---x---":               "x",
		"":                      "",
		"!!!":                   "",
		strings.Repeat("a", 60): strings.Repeat("a", MaxLen),
	}
	for in, want := range cases {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	for _, ok := range []string{"a", "brave-fox", "agent7", "x-y-z"} {
		if good, why := Validate(ok); !good {
			t.Errorf("%q rejected: %s", ok, why)
		}
	}
	for _, bad := range []string{"", "-a", "a-", "Brave", "has space", "user", "ALL", "relay", "me", strings.Repeat("a", 33), "a_b"} {
		if good, _ := Validate(bad); good {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestEveryGeneratorProducesValidNames(t *testing.T) {
	n := New()
	for _, g := range n.gens {
		for i := 0; i < 300; i++ {
			name := g.Generate()
			if name == "" {
				continue // sanitised away; Candidate retries
			}
			if ok, why := Validate(name); !ok && !Reserved[name] {
				t.Fatalf("%s produced invalid name %q: %s", g.ID(), name, why)
			}
		}
	}
}

func TestCandidateAlwaysValidAndFallbackIsUnique(t *testing.T) {
	n := New()
	for attempt := 0; attempt < 30; attempt++ {
		for i := 0; i < 50; i++ {
			if name := n.Candidate(attempt); !validName(name) {
				t.Fatalf("attempt %d gave invalid %q", attempt, name)
			}
		}
	}
	// Past the random attempts, names carry a random suffix: collisions among
	// thousands of draws must be vanishingly rare.
	seen := map[string]bool{}
	dups := 0
	for i := 0; i < 5000; i++ {
		name := n.Candidate(randomAttempts)
		if seen[name] {
			dups++
		}
		seen[name] = true
	}
	if dups > 25 { // 32 x 32 x 31^4 space: birthday collisions are ~0
		t.Errorf("too many fallback collisions: %d", dups)
	}
}

func validName(s string) bool { ok, _ := Validate(s); return ok }

func TestNamerToleratesEmptyGenerator(t *testing.T) {
	n := With(empty{})
	if name := n.Candidate(0); !validName(name) {
		t.Errorf("got %q", name)
	}
}

type empty struct{}

func (empty) ID() string       { return "empty" }
func (empty) Generate() string { return "" }
