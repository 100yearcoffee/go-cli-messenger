package access

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

type Gate struct{ key []byte }

func New(key []byte) (*Gate, error) {
	if len(key) < 24 || len(key) > 1024 {
		return nil, errors.New("access key must be between 24 and 1024 bytes")
	}
	copyKey := append([]byte(nil), key...)
	return &Gate{key: copyKey}, nil
}

func (g *Gate) Authorize(request *http.Request) bool {
	if g == nil {
		return true
	}
	value := request.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return false
	}
	candidate := []byte(strings.TrimPrefix(value, "Bearer "))
	return len(candidate) == len(g.key) && subtle.ConstantTimeCompare(candidate, g.key) == 1
}
