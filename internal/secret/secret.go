// Package secret encrypts SNMP credentials at rest with AES-256-GCM.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

const prefix = "enc:v1:"

// Box encrypts and decrypts short strings and signs session tokens.
type Box struct {
	aead    cipher.AEAD
	signKey []byte
}

// LoadKey returns a 32-byte key from a base64 string, or from keyFile,
// creating keyFile with a random key when it does not exist yet.
func LoadKey(b64, keyFile string) ([]byte, error) {
	if b64 != "" {
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil || len(k) != 32 {
			return nil, errors.New("ZIROX_SECRET_KEY must be 32 bytes, base64-encoded")
		}
		return k, nil
	}
	data, err := os.ReadFile(keyFile)
	if err == nil {
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if err != nil || len(k) != 32 {
			return nil, fmt.Errorf("key file %s is invalid", keyFile)
		}
		return k, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(k)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write key file: %w", err)
	}
	return k, nil
}

// New builds a Box from a 32-byte master key.
func New(key []byte) (*Box, error) {
	block, err := aes.NewCipher(derive(key, "zirox-credentials"))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead, signKey: derive(key, "zirox-sessions")}, nil
}

func derive(key []byte, purpose string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(purpose))
	return m.Sum(nil)
}

// Encrypt returns an opaque string; empty input stays empty.
func (b *Box) Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return prefix + base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt reverses Encrypt.
func (b *Box) Decrypt(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	if !strings.HasPrefix(enc, prefix) {
		return "", errors.New("value is not encrypted")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(enc, prefix))
	if err != nil {
		return "", err
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	plain, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", errors.New("cannot decrypt (wrong secret key?)")
	}
	return string(plain), nil
}

// Sign returns an HMAC of msg for session cookies.
func (b *Box) Sign(msg string) string {
	m := hmac.New(sha256.New, b.signKey)
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Verify checks a signature produced by Sign.
func (b *Box) Verify(msg, sig string) bool {
	return hmac.Equal([]byte(b.Sign(msg)), []byte(sig))
}
