package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoopbackListenDetection(t *testing.T) {
	for _, value := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:8080"} {
		if !isLoopbackListen(value) {
			t.Errorf("%s is not loopback", value)
		}
	}
	for _, value := range []string{"0.0.0.0:8080", "[::]:8080", "192.0.2.1:8080", "bad"} {
		if isLoopbackListen(value) {
			t.Errorf("%s is loopback", value)
		}
	}
}

func TestAccessKeyFileOverridesEnvironmentAndChecksPermissions(t *testing.T) {
	t.Setenv("TERMCALL_ACCESS_KEY", "environment access material")
	value, err := loadAccessKey("")
	if err != nil || string(value) != "environment access material" {
		t.Fatalf("environment = %q, %v", value, err)
	}
	path := filepath.Join(t.TempDir(), "access-key")
	if err := os.WriteFile(path, []byte("explicit access key material\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err = loadAccessKey(path)
	if err != nil || string(value) != "explicit access key material" {
		t.Fatalf("file = %q, %v", value, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAccessKey(path); err == nil {
		t.Fatal("broad key permissions accepted")
	}
}
