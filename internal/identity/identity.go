package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"termcall/internal/protocol"
)

const (
	identityVersion = 2
	proofLifetime   = 2 * time.Minute
)

// Identity is a locally-owned Ed25519 identity. Device is retained as an alias
// for source compatibility with the media/client packages.
type Identity struct {
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
}

type Device = Identity

type storedIdentity struct {
	Version    int    `json:"version"`
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"`
}

func Generate() (*Identity, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate Ed25519 identity: %w", err)
	}
	return &Identity{PublicKey: publicKey, PrivateKey: privateKey}, nil
}

func DefaultPath() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "termcall", "identity.json"), nil
}

func Load() (*Identity, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return LoadFile(path)
}

func LoadFile(path string) (*Identity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("termcall identity file permissions are too broad; expected 0600")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var stored storedIdentity
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return nil, fmt.Errorf("decode identity: %w", err)
	}
	if stored.Version != identityVersion {
		return nil, fmt.Errorf("unsupported identity version %d; protocol v2 does not accept legacy identity files", stored.Version)
	}
	publicKey, err := DecodePublicKey(stored.PublicKey)
	if err != nil {
		return nil, errors.New("saved identity public key is invalid")
	}
	privateKey, err := base64.RawURLEncoding.DecodeString(stored.PrivateKey)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("saved identity private key is invalid")
	}
	if !ed25519.PublicKey(privateKey[32:]).Equal(publicKey) {
		return nil, errors.New("saved identity key pair does not match")
	}
	return &Identity{PublicKey: publicKey, PrivateKey: ed25519.PrivateKey(privateKey)}, nil
}

// SaveNew writes a new identity and refuses to replace any existing key file.
func SaveNew(value *Identity) error {
	path, err := DefaultPath()
	if err != nil {
		return err
	}
	return SaveNewFile(path, value)
}

func SaveNewFile(path string, value *Identity) error {
	if err := validateIdentity(value); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(storedIdentity{
		Version: identityVersion, PublicKey: EncodePublicKey(value.PublicKey),
		PrivateKey: base64.RawURLEncoding.EncodeToString(value.PrivateKey),
	}, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return errors.New("refusing to overwrite existing termcall private key")
		}
		return err
	}
	if _, err = file.Write(encoded); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	return closeErr
}

func validateIdentity(value *Identity) error {
	if value == nil || len(value.PublicKey) != ed25519.PublicKeySize || len(value.PrivateKey) != ed25519.PrivateKeySize ||
		!ed25519.PublicKey(value.PrivateKey[32:]).Equal(value.PublicKey) {
		return errors.New("complete Ed25519 identity is required")
	}
	return nil
}

func (i *Identity) Sign(messageType protocol.SignalType, callID, from, to string, now time.Time) (protocol.IdentityProof, error) {
	if err := validateIdentity(i); err != nil {
		return protocol.IdentityProof{}, err
	}
	proof := protocol.IdentityProof{
		PublicKey: EncodePublicKey(i.PublicKey), ExpiresAt: now.UTC().Add(proofLifetime), Nonce: uuid.NewString(),
	}
	proof.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(i.PrivateKey, proofBytes(messageType, callID, from, to, proof)))
	return proof, nil
}

// SignCall is a compatibility name for signing protocol identity proofs.
func (i *Identity) SignCall(messageType protocol.SignalType, callID, from, to string, now time.Time) (protocol.IdentityProof, error) {
	return i.Sign(messageType, callID, from, to, now)
}

func Verify(message protocol.SignalMessage, now time.Time) (protocol.IdentityProof, ed25519.PublicKey, string, error) {
	var proof protocol.IdentityProof
	decoder := json.NewDecoder(bytes.NewReader(message.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proof); err != nil {
		return proof, nil, "", errors.New("identity proof is required")
	}
	publicKey, err := DecodePublicKey(proof.PublicKey)
	if err != nil {
		return proof, nil, "", err
	}
	fingerprint := Fingerprint(publicKey)
	if now.After(proof.ExpiresAt) || proof.ExpiresAt.After(now.Add(proofLifetime+time.Minute)) {
		return proof, nil, fingerprint, errors.New("identity proof is expired or too far in the future")
	}
	if !AddressMatchesKey(message.From, publicKey) {
		return proof, nil, fingerprint, errors.New("canonical sender suffix does not match public key")
	}
	signature, err := base64.RawURLEncoding.DecodeString(proof.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return proof, nil, fingerprint, errors.New("invalid identity signature encoding")
	}
	if !ed25519.Verify(publicKey, proofBytes(message.Type, message.CallID, message.From, message.To, proof), signature) {
		return proof, nil, fingerprint, errors.New("invalid identity signature")
	}
	return proof, publicKey, fingerprint, nil
}

func VerifyCall(_ ed25519.PublicKey, message protocol.SignalMessage, now time.Time) (protocol.IdentityProof, error) {
	proof, _, _, err := Verify(message, now)
	return proof, err
}

func proofBytes(messageType protocol.SignalType, callID, from, to string, proof protocol.IdentityProof) []byte {
	return []byte(strings.Join([]string{
		strconv.Itoa(protocol.ProtocolVersion), string(messageType), callID, from, to, proof.PublicKey,
		proof.ExpiresAt.UTC().Format(time.RFC3339Nano), proof.Nonce,
	}, "\x00"))
}

func EncodePublicKey(key ed25519.PublicKey) string { return base64.RawURLEncoding.EncodeToString(key) }

func DecodePublicKey(value string) (ed25519.PublicKey, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, errors.New("invalid Ed25519 public key")
	}
	return ed25519.PublicKey(decoded), nil
}

func Fingerprint(key ed25519.PublicKey) string {
	hash := sha256.Sum256(key)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(hash[:]))
}

func CanonicalAddress(baseName string, key ed25519.PublicKey) (string, error) {
	if !protocol.ValidBaseName(baseName) {
		return "", fmt.Errorf("invalid base name %q", baseName)
	}
	fingerprint := Fingerprint(key)
	return baseName + "-" + fingerprint[:protocol.AddressSuffixLength], nil
}

func AddressMatchesKey(address string, key ed25519.PublicKey) bool {
	return protocol.ValidAddress(address) && strings.HasSuffix(address, "-"+Fingerprint(key)[:protocol.AddressSuffixLength])
}

func SafetyCode(local, remote ed25519.PublicKey, callID string) string {
	first, second := string(local), string(remote)
	if first > second {
		first, second = second, first
	}
	hash := sha256.Sum256([]byte(first + second + callID))
	return strings.ToUpper(fmt.Sprintf("%x", hash[:5]))
}
