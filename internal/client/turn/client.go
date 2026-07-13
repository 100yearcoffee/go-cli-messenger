package turn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"termcall/internal/protocol"
)

type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string { return e.Message }

func Credentials(ctx context.Context, serverURL, accessKey string) (protocol.TURNCredentials, error) {
	var result protocol.TURNCredentials
	base, err := apiBase(serverURL)
	if err != nil {
		return result, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/turn-credentials", nil)
	if err != nil {
		return result, err
	}
	if accessKey != "" {
		request.Header.Set("Authorization", "Bearer "+accessKey)
	}
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		return result, fmt.Errorf("TURN credential request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return result, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := response.Status
		var value struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &value)
		if value.Error != "" {
			message = value.Error
		}
		return result, &APIError{StatusCode: response.StatusCode, Message: message}
	}
	if len(bytes.TrimSpace(body)) != 0 {
		if err := json.Unmarshal(body, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

func apiBase(serverURL string) (string, error) {
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "ws":
		if parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" {
			return "", errors.New("credentials require wss:// for non-local servers")
		}
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	default:
		return "", errors.New("signaling URL must use ws:// or wss://")
	}
	parsed.RawQuery, parsed.Fragment = "", ""
	parsed.Path = strings.TrimSuffix(parsed.Path, "/v1/ws")
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}
