// Package secrets encrypts collector API tokens at rest — the only secrets
// HRG ever holds. Credentials for documented systems are never stored;
// annotations point at where they live instead.
//
// AES-256-GCM with a random key in a 0600 file next to the database. If the
// key file is lost, stored tokens are unrecoverable and must be re-entered —
// by design, that is the entire blast radius.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

const keySize = 32

// Key is the symmetric key used to seal and open tokens.
type Key struct {
	bytes []byte
}

// LoadOrCreateKey reads the hex-encoded key at path, generating one with
// 0600 permissions if the file does not exist.
func LoadOrCreateKey(path string) (*Key, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		k := make([]byte, keySize)
		if _, err := rand.Read(k); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(k)+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("write key file %s: %w", path, err)
		}
		return &Key{bytes: k}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read key file %s: %w", path, err)
	}
	k, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(k) != keySize {
		return nil, fmt.Errorf("key file %s: expected %d hex-encoded bytes", path, keySize)
	}
	return &Key{bytes: k}, nil
}

// Seal encrypts plaintext; the nonce is prepended to the returned blob.
func (k *Key) Seal(plaintext string) ([]byte, error) {
	gcm, err := k.gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Open decrypts a blob produced by Seal.
func (k *Key) Open(blob []byte) (string, error) {
	gcm, err := k.gcm()
	if err != nil {
		return "", err
	}
	if len(blob) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	plain, err := gcm.Open(nil, blob[:gcm.NonceSize()], blob[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plain), nil
}

func (k *Key) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(k.bytes)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
