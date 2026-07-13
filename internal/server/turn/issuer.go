package turn

import (
	"crypto/hmac"
	"crypto/sha1" // coturn's TURN REST API intentionally specifies HMAC-SHA1.
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"

	"termcall/internal/protocol"
)

type Config struct {
	Secret   []byte
	URLs     []string
	STUNURLs []string
	TTL      time.Duration
	Now      func() time.Time
}

type Issuer struct{ config Config }

func New(config Config) (*Issuer, error) {
	if len(config.Secret) < 32 {
		return nil, errors.New("TURN shared secret must contain at least 32 bytes")
	}
	if len(config.URLs) == 0 {
		return nil, errors.New("at least one TURN URL is required")
	}
	if len(config.URLs) > 8 || len(config.STUNURLs) > 8 {
		return nil, errors.New("too many ICE server URLs")
	}
	for _, value := range config.URLs {
		if !protocol.ValidTURNURL(value) {
			return nil, fmt.Errorf("invalid TURN URL %q", value)
		}
	}
	for _, value := range config.STUNURLs {
		if !protocol.ValidSTUNURL(value) {
			return nil, fmt.Errorf("invalid STUN URL %q", value)
		}
	}
	if config.TTL <= 0 {
		config.TTL = 10 * time.Minute
	}
	if config.TTL < time.Minute || config.TTL > time.Hour {
		return nil, errors.New("TURN credential lifetime must be between 1 minute and 1 hour")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	config.Secret = append([]byte(nil), config.Secret...)
	config.URLs = append([]string(nil), config.URLs...)
	config.STUNURLs = append([]string(nil), config.STUNURLs...)
	return &Issuer{config: config}, nil
}

func (i *Issuer) Issue(username string) protocol.TURNCredentials {
	expiresAt := i.config.Now().UTC().Add(i.config.TTL)
	turnUsername := strconv.FormatInt(expiresAt.Unix(), 10) + ":" + username
	mac := hmac.New(sha1.New, i.config.Secret)
	_, _ = mac.Write([]byte(turnUsername))
	credential := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	servers := make([]protocol.ICEServer, 0, 2)
	if len(i.config.STUNURLs) != 0 {
		servers = append(servers, protocol.ICEServer{URLs: append([]string(nil), i.config.STUNURLs...)})
	}
	servers = append(servers, protocol.ICEServer{
		URLs: append([]string(nil), i.config.URLs...), Username: turnUsername, Credential: credential,
	})
	return protocol.TURNCredentials{ExpiresAt: expiresAt, ICEServers: servers}
}
