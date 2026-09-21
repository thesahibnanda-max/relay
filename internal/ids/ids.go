// Package ids generates ULIDs: sortable, unique, safe to type and paste.
package ids

import (
	"crypto/rand"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

var (
	mu      sync.Mutex
	entropy = ulid.Monotonic(rand.Reader, 0)
)

// New returns a fresh, monotonically increasing ULID string.
func New() string {
	mu.Lock()
	defer mu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
}

// Valid reports whether s is a well-formed ULID (case-insensitive).
func Valid(s string) bool {
	if len(s) != ulid.EncodedSize {
		return false
	}
	_, err := ulid.ParseStrict(strings.ToUpper(s))
	return err == nil
}

// Normalize upper-cases a ULID so lookups are case-insensitive.
func Normalize(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// Token returns a random 256-bit secret, hex-encoded (resume/auth tokens).
func Token() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // the OS RNG failing is unrecoverable
	}
	const hexd = "0123456789abcdef"
	out := make([]byte, 64)
	for i, v := range b {
		out[i*2], out[i*2+1] = hexd[v>>4], hexd[v&0xf]
	}
	return string(out)
}
