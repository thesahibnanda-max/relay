// Package naming produces human-friendly agent names. Several generators are
// mixed at random for variety, but correctness never depends on them: the
// store enforces uniqueness, and the final fallback appends a random suffix so
// a free name always exists.
package naming

import (
	"math/rand/v2"
	"regexp"
	"strings"

	"github.com/anandvarma/namegen"
	"github.com/icrowley/fake"
)

// MaxLen bounds a name so it stays readable in tables and prompts.
const MaxLen = 32

// Reserved words can't be used as agent names (they mean something in commands).
var Reserved = map[string]bool{"user": true, "all": true, "relay": true, "me": true, "self": true, "none": true, "new": true}

var (
	nonName = regexp.MustCompile(`[^a-z0-9]+`)
	valid   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$|^[a-z0-9]$`)
)

// Sanitize lower-cases and reduces s to [a-z0-9-], with no leading/trailing
// dash, capped at MaxLen. It returns "" if nothing usable remains.
func Sanitize(s string) string {
	s = nonName.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	s = strings.Trim(s, "-")
	if len(s) > MaxLen {
		s = strings.Trim(s[:MaxLen], "-")
	}
	return s
}

// Validate checks a user-supplied name. It returns a human-readable reason.
func Validate(name string) (ok bool, reason string) {
	switch {
	case name == "":
		return false, "name is empty"
	case !valid.MatchString(name):
		return false, "use 1-32 characters: lowercase letters, digits and dashes (not at the ends)"
	case Reserved[name]:
		return false, `"` + name + `" is reserved`
	}
	return true, ""
}

// Generator yields candidate names (not guaranteed unique).
type Generator interface {
	ID() string
	Generate() string
}

type namegenGen struct{ g interface{ Get() string } }

func (namegenGen) ID() string         { return "namegen" }
func (n namegenGen) Generate() string { return Sanitize(n.g.Get()) }

type fakeGen struct{}

func (fakeGen) ID() string       { return "fake" }
func (fakeGen) Generate() string { return Sanitize(fake.Color() + "-" + fake.FirstName()) }

type builtinGen struct{}

func (builtinGen) ID() string { return "builtin" }
func (builtinGen) Generate() string {
	return adjectives[rand.IntN(len(adjectives))] + "-" + nouns[rand.IntN(len(nouns))]
}

// Namer picks generators at random and falls back to a guaranteed-free form.
type Namer struct{ gens []Generator }

// New returns a Namer over all built-in generators.
func New() *Namer {
	return &Namer{gens: []Generator{namegenGen{namegen.New()}, fakeGen{}, builtinGen{}}}
}

// With returns a Namer over exactly the given generators (tests).
func With(g ...Generator) *Namer { return &Namer{gens: g} }

// randomAttempts is how many times we retry random names before falling back.
const randomAttempts = 12

// Candidate returns the name to try on the given attempt (0-based). The first
// randomAttempts come from a random generator; later ones are the builtin
// generator plus a random suffix, which makes a collision astronomically
// unlikely, so callers can loop until the store accepts.
func (n *Namer) Candidate(attempt int) string {
	if attempt < randomAttempts {
		for tries := 0; tries < 3; tries++ { // a generator may sanitise to ""
			if name := n.gens[rand.IntN(len(n.gens))].Generate(); name != "" && !Reserved[name] {
				return name
			}
		}
	}
	base := builtinGen{}.Generate()
	if len(base) > MaxLen-5 {
		base = strings.Trim(base[:MaxLen-5], "-")
	}
	return base + "-" + suffix(4)
}

func suffix(n int) string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rand.IntN(len(alphabet))]
	}
	return string(b)
}

var adjectives = []string{"amber", "brave", "calm", "clever", "cosmic", "crisp", "daring", "eager", "fancy", "gentle", "golden", "happy", "jolly", "keen", "lively", "lucky", "mellow", "nimble", "noble", "plucky", "proud", "quick", "quiet", "rapid", "royal", "shiny", "silent", "sunny", "swift", "tidy", "vivid", "witty"}

var nouns = []string{"badger", "beacon", "comet", "falcon", "fox", "harbor", "heron", "ibex", "jaguar", "kestrel", "lantern", "lynx", "maple", "meteor", "otter", "panda", "pebble", "quasar", "raven", "river", "sparrow", "summit", "tiger", "willow", "wolf", "zephyr", "cedar", "delta", "ember", "glacier", "orbit", "prairie"}
