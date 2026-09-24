package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// newToken returns a fresh 256-bit random resume token and its hash. Only
// the hash is ever persisted (mirroring the local daemon's own convention) -
// the token itself is handed to the caller exactly once, at registration.
func newToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = hex.EncodeToString(b)
	return token, hashToken(token), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
