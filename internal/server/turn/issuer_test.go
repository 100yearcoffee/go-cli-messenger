package turn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"testing"
	"time"
)

func TestIssueCoturnRESTCredential(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	secret := []byte("0123456789abcdef0123456789abcdef")
	issuer, err := New(Config{
		Secret: secret, URLs: []string{"turn:turn.example.test:3478?transport=udp", "turns:turn.example.test:5349"},
		STUNURLs: []string{"stun:turn.example.test:3478"}, TTL: 10 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	credentials := issuer.Issue("alice")
	wantUsername := "1700000600:alice"
	mac := hmac.New(sha1.New, secret)
	_, _ = mac.Write([]byte(wantUsername))
	wantCredential := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	turnServer := credentials.ICEServers[1]
	if turnServer.Username != wantUsername || turnServer.Credential != wantCredential {
		t.Fatalf("TURN credential = %#v, want username %q credential %q", turnServer, wantUsername, wantCredential)
	}
}
