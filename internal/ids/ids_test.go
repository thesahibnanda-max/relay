package ids

import (
	"sort"
	"sync"
	"testing"
)

func TestNewIsValidUniqueAndOrdered(t *testing.T) {
	const n = 5000
	got := make([]string, n)
	for i := range got {
		got[i] = New()
		if !Valid(got[i]) {
			t.Fatalf("invalid ULID %q", got[i])
		}
	}
	if !sort.StringsAreSorted(got) {
		t.Error("ULIDs from one goroutine must sort in creation order")
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("duplicate %s", id)
		}
		seen[id] = true
	}
}

func TestNewIsUniqueAcrossGoroutines(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				id := New()
				mu.Lock()
				if seen[id] {
					t.Errorf("duplicate %s", id)
				}
				seen[id] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
}

func TestValidAndNormalize(t *testing.T) {
	id := New()
	if !Valid(id) || !Valid(lower(id)) {
		t.Error("valid ULID (any case) rejected")
	}
	for _, bad := range []string{"", "NEW", "abc", id + "0", "ZZZZZZZZZZZZZZZZZZZZZZZZZZ", "01ARZ3NDEKTSV4RRFFQ69G5FAI"} { // last has an 'I' (not in alphabet)
		if Valid(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	if Normalize("  "+lower(id)+" ") != id {
		t.Error("Normalize failed")
	}
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func TestToken(t *testing.T) {
	a, b := Token(), Token()
	if len(a) != 64 || a == b {
		t.Errorf("token bad: %q %q", a, b)
	}
}
