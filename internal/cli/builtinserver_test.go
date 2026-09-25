package cli

import "testing"

// setBuiltinServerURL sets the package-level var for the duration of one
// test and restores it afterward - builtinServerURL is empty in every
// ordinary test build (no release ldflag), so tests that need it set must
// clean up after themselves for the other, unrelated tests in this package.
func setBuiltinServerURL(t *testing.T, v string) {
	t.Helper()
	old := builtinServerURL
	builtinServerURL = v
	t.Cleanup(func() { builtinServerURL = old })
}

func TestBuiltinServer_EmptyMeansNoBuiltinServer(t *testing.T) {
	setBuiltinServerURL(t, "")
	if _, ok := builtinServer(); ok {
		t.Fatal("expected ok=false when builtinServerURL is empty")
	}
}

func TestBuiltinServer_BareHostPortPassesThrough(t *testing.T) {
	setBuiltinServerURL(t, "relay.example.com:443")
	hostPort, ok := builtinServer()
	if !ok {
		t.Fatal("expected ok=true")
	}
	if want := "relay.example.com:443"; hostPort != want {
		t.Errorf("hostPort = %q, want %q", hostPort, want)
	}
}

func TestBuiltinServer_FullURLHasSchemeStripped(t *testing.T) {
	setBuiltinServerURL(t, "https://relay.example.com:443")
	hostPort, ok := builtinServer()
	if !ok {
		t.Fatal("expected ok=true")
	}
	if want := "relay.example.com:443"; hostPort != want {
		t.Errorf("hostPort = %q, want %q", hostPort, want)
	}
}

func TestBuiltinServer_NoPortDefaultsTo443(t *testing.T) {
	setBuiltinServerURL(t, "relay.example.com")
	hostPort, ok := builtinServer()
	if !ok {
		t.Fatal("expected ok=true")
	}
	if want := "relay.example.com:443"; hostPort != want {
		t.Errorf("hostPort = %q, want %q", hostPort, want)
	}
}

func TestResolveGlobalServer_NoBuiltinNoRequestPassesThrough(t *testing.T) {
	setBuiltinServerURL(t, "")
	hostPort, forceTLS, err := resolveGlobalServer("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hostPort != "" || forceTLS {
		t.Errorf("hostPort=%q forceTLS=%v, want empty/false (today's exact local-build behavior)", hostPort, forceTLS)
	}
}

func TestResolveGlobalServer_NoBuiltinRequestPassesThroughUnchanged(t *testing.T) {
	setBuiltinServerURL(t, "")
	hostPort, forceTLS, err := resolveGlobalServer("myhost:9999")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hostPort != "myhost:9999" || forceTLS {
		t.Errorf("hostPort=%q forceTLS=%v, want %q/false", hostPort, forceTLS, "myhost:9999")
	}
}

func TestResolveGlobalServer_BuiltinNoRequestUsesBuiltinWithTLS(t *testing.T) {
	setBuiltinServerURL(t, "relay.example.com:443")
	hostPort, forceTLS, err := resolveGlobalServer("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hostPort != "relay.example.com:443" || !forceTLS {
		t.Errorf("hostPort=%q forceTLS=%v, want the builtin server with TLS forced", hostPort, forceTLS)
	}
}

func TestResolveGlobalServer_BuiltinMatchingRequestSucceeds(t *testing.T) {
	setBuiltinServerURL(t, "relay.example.com:443")
	hostPort, forceTLS, err := resolveGlobalServer("relay.example.com:443")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hostPort != "relay.example.com:443" || !forceTLS {
		t.Errorf("hostPort=%q forceTLS=%v, want the builtin server with TLS forced", hostPort, forceTLS)
	}
}

func TestResolveGlobalServer_BuiltinMismatchedRequestIsRejected(t *testing.T) {
	setBuiltinServerURL(t, "relay.example.com:443")
	_, _, err := resolveGlobalServer("someone-elses-server.com:5555")
	if err == nil {
		t.Fatal("expected an error rejecting the mismatched server, got nil")
	}
}
