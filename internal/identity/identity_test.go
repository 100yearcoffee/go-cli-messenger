package identity

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"termcall/internal/protocol"
)

func TestFingerprintAndCanonicalAddressAreDeterministic(t *testing.T) {
	value, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := Fingerprint(value.PublicKey)
	if fingerprint != Fingerprint(value.PublicKey) || len(fingerprint) != 52 || fingerprint != strings.ToLower(fingerprint) || strings.Contains(fingerprint, "=") {
		t.Fatalf("fingerprint = %q", fingerprint)
	}
	address, err := CanonicalAddress("alice_dev", value.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if address != "alice_dev-"+fingerprint[:12] || !AddressMatchesKey(address, value.PublicKey) {
		t.Fatalf("address = %q", address)
	}
	other, _ := Generate()
	if AddressMatchesKey(address, other.PublicKey) {
		t.Fatal("different key matched address")
	}
}

func TestSignedProofRejectsTamperingExpiryAndWrongSuffix(t *testing.T) {
	value, _ := Generate()
	now := time.Unix(1000, 0).UTC()
	from, _ := CanonicalAddress("alice", value.PublicKey)
	other, _ := Generate()
	to, _ := CanonicalAddress("bob", other.PublicKey)
	message := protocol.SignalMessage{Version: 2, ID: uuid.NewString(), Type: protocol.SignalCallInvite, Timestamp: now, CallID: uuid.NewString(), From: from, To: to}
	proof, err := value.Sign(message.Type, message.CallID, message.From, message.To, now)
	if err != nil {
		t.Fatal(err)
	}
	message.Payload, _ = json.Marshal(proof)
	if _, _, _, err := Verify(message, now); err != nil {
		t.Fatalf("valid proof rejected: %v", err)
	}
	tampered := message
	tampered.To = from
	if _, _, _, err := Verify(tampered, now); err == nil {
		t.Fatal("tampered recipient accepted")
	}
	if _, _, _, err := Verify(message, proof.ExpiresAt.Add(time.Second)); err == nil {
		t.Fatal("expired proof accepted")
	}
	tampered = message
	tampered.From = "mallory-" + strings.Repeat("a", 12)
	if _, _, _, err := Verify(tampered, now); err == nil {
		t.Fatal("wrong suffix accepted")
	}
}

func TestIdentitySaveRefusesReplacementAndRejectsLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	value, _ := Generate()
	if err := SaveNewFile(path, value); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	if err := SaveNewFile(path, value); err == nil {
		t.Fatal("identity was overwritten")
	}
	if _, err := LoadFile(path); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(legacy, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(legacy); err == nil {
		t.Fatal("legacy identity accepted")
	}
}

func TestTrustStoreObservationResolutionAndNoImplicitTrust(t *testing.T) {
	store, _ := OpenTrustStoreFile(filepath.Join(t.TempDir(), "trust.json"))
	first, _ := Generate()
	address, _ := CanonicalAddress("alice", first.PublicKey)
	record, reused, err := store.Observe("wss://one.example/v1/ws", address, first.PublicKey)
	if err != nil || len(reused) != 0 || record.Trusted {
		t.Fatalf("observe = %+v, %v, %v", record, reused, err)
	}
	resolved, err := store.Resolve(record.Fingerprint[:16])
	if err != nil || resolved != record.Fingerprint {
		t.Fatalf("resolve = %q, %v", resolved, err)
	}
	trusted, err := store.SetTrusted(record.Fingerprint[:16], true)
	if err != nil || !trusted.Trusted {
		t.Fatalf("trust = %+v, %v", trusted, err)
	}
	pretrusted := strings.Repeat("a", 52)
	if _, err := store.SetTrusted(pretrusted, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve("zzzz"); !errors.Is(err, ErrUnknownFingerprint) {
		t.Fatalf("unknown = %v", err)
	}
	store.records[strings.Repeat("b", 52)] = TrustRecord{Fingerprint: strings.Repeat("b", 52)}
	store.records["b"+strings.Repeat("c", 51)] = TrustRecord{Fingerprint: "b" + strings.Repeat("c", 51)}
	if _, err := store.Resolve("b"); !errors.Is(err, ErrAmbiguousFingerprint) {
		t.Fatalf("ambiguous = %v", err)
	}
	second, _ := Generate()
	_, reused, err = store.Observe("wss://one.example/v1/ws", address, second.PublicKey)
	if err != nil || len(reused) != 1 || reused[0] != record.Fingerprint {
		t.Fatalf("alias reuse = %v, %v", reused, err)
	}
}

func TestReplayGuard(t *testing.T) {
	var guard ReplayGuard
	now := time.Now()
	if !guard.Accept("fingerprint", "nonce", now.Add(time.Minute), now) || guard.Accept("fingerprint", "nonce", now.Add(time.Minute), now) {
		t.Fatal("replay guard failed")
	}
}
