package access

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGateValidatesLengthAndBearer(t *testing.T) {
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := New([]byte(strings.Repeat("x", 1025))); err == nil {
		t.Fatal("long key accepted")
	}
	gate, err := New([]byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/v1/ws", nil)
	if gate.Authorize(request) {
		t.Fatal("missing key accepted")
	}
	request.Header.Set("Authorization", "Bearer wrong wrong wrong wrong")
	if gate.Authorize(request) {
		t.Fatal("wrong key accepted")
	}
	request.Header.Set("Authorization", "Bearer correct horse battery staple")
	if !gate.Authorize(request) {
		t.Fatal("correct key rejected")
	}
	var open *Gate
	if !open.Authorize(request) {
		t.Fatal("open gate rejected request")
	}
}
