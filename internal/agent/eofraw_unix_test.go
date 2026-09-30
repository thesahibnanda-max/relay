//go:build unix

package agent

import (
	"os"
	"testing"
	"time"
)

// A full-screen tool puts its terminal in raw mode a moment after it starts.
// Piped stdin that ends before then must still end it: an EOF key typed
// while the terminal was canonical is eaten by the line discipline, so it
// is typed again once the tool reads raw keys (seen on Linux CI with agy).
func TestPipedEOFReachesAToolThatGoesRawLate(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.Close() // stdin has already ended when the tool starts
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	done := make(chan int, 1)
	go func() {
		// Raw after a delay, then exit on the first Ctrl+D (Linux hands a raw
		// reader NUL for an EOF typed while canonical: not a key).
		script := `sleep 0.5; stty raw -echo; while :; do c=$(dd bs=1 count=1 2>/dev/null | od -An -tx1 | tr -d ' '); [ "$c" = 04 ] && exit 0; done`
		code, _ := Run(Config{Tool: "test", Bin: "sh", Args: []string{"-c", script},
			Env: os.Environ(), Interceptor: &recorder{}, In: r, Out: out})
		done <- code
	}()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the tool never got an EOF key after going raw")
	}
}
