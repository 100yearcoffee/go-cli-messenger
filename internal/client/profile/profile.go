package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"termcall/internal/identity"
	"termcall/internal/protocol"
)

const Version = 2

type Profile struct {
	Version   int    `json:"version"`
	BaseName  string `json:"base_name"`
	Address   string `json:"address"`
	ServerURL string `json:"server_url"`
	AccessKey string `json:"access_key,omitempty"`
}

func DefaultPath() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "termcall", "profile.json"), nil
}

func Load() (Profile, error) {
	path, err := DefaultPath()
	if err != nil {
		return Profile{}, err
	}
	return LoadFile(path)
}

func LoadFile(path string) (Profile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Profile{}, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Profile{}, errors.New("termcall profile permissions are too broad; expected 0600")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, err
	}
	var value Profile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Profile{}, fmt.Errorf("decode profile: %w", err)
	}
	if err := Validate(value); err != nil {
		return Profile{}, err
	}
	return value, nil
}

func Validate(value Profile) error {
	if value.Version != Version {
		return errors.New("legacy or unsupported profile; protocol v2 requires a new profile")
	}
	if !protocol.ValidBaseName(value.BaseName) || !protocol.ValidAddress(value.Address) || value.ServerURL == "" {
		return errors.New("saved profile is incomplete or malformed")
	}
	if value.AccessKey != "" && (len(value.AccessKey) < 24 || len(value.AccessKey) > 1024) {
		return errors.New("access key must be between 24 and 1024 bytes")
	}
	return nil
}

func ValidateWithIdentity(value Profile, key *identity.Identity) error {
	if err := Validate(value); err != nil {
		return err
	}
	expected, err := identity.CanonicalAddress(value.BaseName, key.PublicKey)
	if err != nil || expected != value.Address {
		return errors.New("profile address does not match the local identity")
	}
	return nil
}

func Save(value Profile) error {
	path, err := DefaultPath()
	if err != nil {
		return err
	}
	return SaveFile(path, value)
}

func SaveFile(path string, value Profile) error {
	if err := Validate(value); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".profile-*")
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
	return os.Rename(temporaryPath, path)
}

func AccessKeyFromEnvironment() (string, error) {
	if file := os.Getenv("TERMCALL_ACCESS_KEY_FILE"); file != "" {
		info, err := os.Stat(file)
		if err != nil {
			return "", err
		}
		if info.Mode().Perm()&0o077 != 0 {
			return "", errors.New("access key file must not be accessible by group or other users")
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}
	return os.Getenv("TERMCALL_ACCESS_KEY"), nil
}
