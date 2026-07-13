package identity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const trustVersion = 2

var (
	ErrAmbiguousFingerprint = errors.New("fingerprint prefix is ambiguous")
	ErrUnknownFingerprint   = errors.New("fingerprint prefix has not been observed")
)

type Alias struct {
	ServerURL string `json:"server_url"`
	Address   string `json:"address"`
}

type TrustRecord struct {
	Fingerprint string  `json:"fingerprint"`
	Trusted     bool    `json:"trusted,omitempty"`
	Blocked     bool    `json:"blocked,omitempty"`
	PublicKey   string  `json:"public_key,omitempty"`
	Aliases     []Alias `json:"aliases,omitempty"`
}

type storedTrust struct {
	Version int                    `json:"version"`
	Records map[string]TrustRecord `json:"records"`
}

type TrustStore struct {
	mu      sync.Mutex
	path    string
	records map[string]TrustRecord
}

func OpenTrustStore() (*TrustStore, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return OpenTrustStoreFile(filepath.Join(directory, "termcall", "trust.json"))
}

func OpenTrustStoreFile(path string) (*TrustStore, error) {
	store := &TrustStore{path: path, records: make(map[string]TrustRecord)}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("termcall trust file permissions are too broad; expected 0600")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var saved storedTrust
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&saved); err != nil {
		return nil, fmt.Errorf("decode trust store: %w", err)
	}
	if saved.Version != trustVersion || saved.Records == nil {
		return nil, errors.New("legacy or malformed trust store is not accepted by protocol v2")
	}
	store.records = saved.Records
	return store, nil
}

// Observe records a verified key and alias without implicitly trusting it.
// It returns the record and fingerprints previously seen using the same alias.
func (s *TrustStore) Observe(serverURL, address string, publicKey ed25519.PublicKey) (TrustRecord, []string, error) {
	fingerprint := Fingerprint(publicKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.records[fingerprint]
	record.Fingerprint = fingerprint
	encodedKey := EncodePublicKey(publicKey)
	if record.PublicKey != "" && record.PublicKey != encodedKey {
		return TrustRecord{}, nil, errors.New("fingerprint/public-key inconsistency")
	}
	record.PublicKey = encodedKey
	alias := Alias{ServerURL: serverURL, Address: address}
	if !containsAlias(record.Aliases, alias) {
		record.Aliases = append(record.Aliases, alias)
	}
	var reused []string
	for otherFingerprint, other := range s.records {
		if otherFingerprint != fingerprint && containsAlias(other.Aliases, alias) {
			reused = append(reused, otherFingerprint)
		}
	}
	sort.Strings(reused)
	s.records[fingerprint] = record
	return record, reused, s.saveLocked()
}

func (s *TrustStore) Resolve(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if !validFingerprintText(value) {
		return "", errors.New("fingerprint must contain lowercase base32 characters")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(value) == 52 {
		return value, nil
	}
	var matches []string
	for fingerprint := range s.records {
		if strings.HasPrefix(fingerprint, value) {
			matches = append(matches, fingerprint)
		}
	}
	if len(matches) == 0 {
		return "", ErrUnknownFingerprint
	}
	if len(matches) != 1 {
		return "", ErrAmbiguousFingerprint
	}
	return matches[0], nil
}

func (s *TrustStore) SetTrusted(value string, trusted bool) (TrustRecord, error) {
	fingerprint, err := s.Resolve(value)
	if err != nil {
		return TrustRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.records[fingerprint]
	record.Fingerprint, record.Trusted = fingerprint, trusted
	s.records[fingerprint] = record
	return record, s.saveLocked()
}

func (s *TrustStore) SetBlockedFingerprint(value string, blocked bool) (TrustRecord, error) {
	fingerprint, err := s.Resolve(value)
	if err != nil {
		return TrustRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.records[fingerprint]
	record.Fingerprint, record.Blocked = fingerprint, blocked
	s.records[fingerprint] = record
	return record, s.saveLocked()
}

func (s *TrustStore) Record(fingerprint string) (TrustRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[fingerprint]
	return record, ok
}

func containsAlias(values []Alias, wanted Alias) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func validFingerprintText(value string) bool {
	if len(value) == 0 || len(value) > 52 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '2' || character > '7') {
			return false
		}
	}
	return true
}

func (s *TrustStore) saveLocked() error {
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(storedTrust{Version: trustVersion, Records: s.records}, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".trust-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, s.path)
}

type ReplayGuard struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func (g *ReplayGuard) Accept(fingerprint, nonce string, expiresAt, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen == nil {
		g.seen = make(map[string]time.Time)
	}
	for key, expiry := range g.seen {
		if !expiry.After(now) {
			delete(g.seen, key)
		}
	}
	key := fingerprint + "\x00" + nonce
	if _, exists := g.seen[key]; exists {
		return false
	}
	if len(g.seen) >= 65536 {
		return false
	}
	g.seen[key] = expiresAt
	return true
}
