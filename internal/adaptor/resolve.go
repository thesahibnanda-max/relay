package adaptor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveBinary finds name on PATH, skipping any entry that is Relay itself.
// That matters in shim mode: if `claude` is a symlink to relay, a plain
// exec.LookPath would find the shim first and Relay would exec itself forever.
func ResolveBinary(name string) (string, error) {
	self, _ := os.Executable()
	return resolveIn(name, os.Getenv("PATH"), self)
}

func resolveIn(name, pathEnv, self string) (string, error) {
	var selfInfo os.FileInfo
	if self != "" {
		if real, err := filepath.EvalSymlinks(self); err == nil {
			self = real
		}
		selfInfo, _ = os.Stat(self)
	}

	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate) // follows symlinks
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		if selfInfo != nil && os.SameFile(info, selfInfo) {
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("%w: %q not found on PATH (excluding relay itself)", ErrNotFound, name)
}

var ErrNotFound = errors.New("binary not found")

// EnvActive is set in the child environment so a nested Relay can detect it.
const EnvActive = "RELAY_ACTIVE"

// ChildEnv builds the child environment: the current one, the adaptor's extras,
// and the nesting marker.
func ChildEnv(a Adaptor) []string {
	env := append(os.Environ(), a.Env()...)
	if !hasKey(env, EnvActive) {
		env = append(env, EnvActive+"=1")
	}
	return env
}

func hasKey(env []string, key string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
}
