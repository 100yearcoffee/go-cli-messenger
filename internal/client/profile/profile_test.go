package profile

import (
	"os"
	"path/filepath"
	"testing"

	"termcall/internal/identity"
)

func TestProfileRoundTripPermissionsAndIdentityBinding(t *testing.T) {
	localIdentity, _ := identity.Generate()
	address, _ := identity.CanonicalAddress("alice", localIdentity.PublicKey)
	value := Profile{Version: Version, BaseName: "alice", Address: address, ServerURL: "wss://signal.example/v1/ws", AccessKey: "correct horse battery staple"}
	path := filepath.Join(t.TempDir(), "profile.json")
	if err := SaveFile(path, value); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != value {
		t.Fatalf("loaded = %+v", loaded)
	}
	if err := ValidateWithIdentity(loaded, localIdentity); err != nil {
		t.Fatal(err)
	}
	other, _ := identity.Generate()
	if err := ValidateWithIdentity(loaded, other); err == nil {
		t.Fatal("profile accepted wrong identity")
	}
}

func TestProfileRejectsLegacyMalformedAndBroadPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(path, []byte(`{"username":"alice","access_token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil {
		t.Fatal("legacy profile accepted")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil {
		t.Fatal("broad permissions accepted")
	}
}

func TestAccessKeyEnvironmentAndSecretFile(t *testing.T) {
	t.Setenv("TERMCALL_ACCESS_KEY", "correct horse battery staple")
	value, err := AccessKeyFromEnvironment()
	if err != nil || value != "correct horse battery staple" {
		t.Fatalf("env = %q, %v", value, err)
	}
	path := filepath.Join(t.TempDir(), "access-key")
	if err := os.WriteFile(path, []byte("file access key material\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMCALL_ACCESS_KEY_FILE", path)
	value, err = AccessKeyFromEnvironment()
	if err != nil || value != "file access key material" {
		t.Fatalf("file = %q, %v", value, err)
	}
}
