package proto

import (
	"encoding/base64"
	"testing"
)

func TestJoinBlobRoundTrips(t *testing.T) {
	want := JoinBlob{Session: "01ARZ3NDEKTSV4RRFFQ69G5FAV", PeerAddr: "tcXXXXXXXXX", PeerName: "laptop", Secret: "s3cr3t"}
	blob := EncodeJoinBlob(want)
	if got := blob[:len(JoinBlobPrefix)]; got != JoinBlobPrefix {
		t.Fatalf("encoded blob does not start with the prefix: %q", got)
	}
	got, err := ParseJoinBlob(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got.Session != want.Session || got.PeerAddr != want.PeerAddr || got.PeerName != want.PeerName || got.Secret != want.Secret {
		t.Fatalf("round trip changed the blob: %+v vs %+v", got, want)
	}
	if got.V != JoinBlobVersion {
		t.Fatalf("V = %d, want %d", got.V, JoinBlobVersion)
	}
}

func TestJoinBlobNormalizesSessionCase(t *testing.T) {
	blob := EncodeJoinBlob(JoinBlob{Session: "01arz3ndektsv4rrffq69g5fav", PeerAddr: "tcX", Secret: "s"})
	got, err := ParseJoinBlob(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got.Session != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Fatalf("session not normalized: %q", got.Session)
	}
}

func TestParseJoinBlobRejectsGarbage(t *testing.T) {
	for _, bad := range []string{
		"",
		"not-a-join-blob-at-all",
		"relay-join-v1.",
		"relay-join-v1.not-valid-base64!!!",
		JoinBlobPrefix + toB64(`{"v":1,"session":"not-a-ulid","peer_addr":"tc","secret":"s"}`),
		JoinBlobPrefix + toB64(`{"v":1,"session":"01ARZ3NDEKTSV4RRFFQ69G5FAV","peer_addr":"","secret":"s"}`),
		JoinBlobPrefix + toB64(`{"v":1,"session":"01ARZ3NDEKTSV4RRFFQ69G5FAV","peer_addr":"tc","secret":""}`),
		JoinBlobPrefix + toB64(`{"v":99,"session":"01ARZ3NDEKTSV4RRFFQ69G5FAV","peer_addr":"tc","secret":"s"}`),
		JoinBlobPrefix + toB64(`not json`),
	} {
		if _, err := ParseJoinBlob(bad); err == nil {
			t.Errorf("expected an error for %q", bad)
		}
	}
}

func toB64(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
