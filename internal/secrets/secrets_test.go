package secrets

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hrg.key")
	k, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}

	blob, err := k.Seal("PVEAPIToken-secret-value")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("secret-value")) {
		t.Fatal("ciphertext contains plaintext")
	}

	got, err := k.Open(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got != "PVEAPIToken-secret-value" {
		t.Fatalf("roundtrip mismatch: %q", got)
	}

	// Same key file reloads to a working key.
	k2, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := k2.Open(blob); err != nil || got != "PVEAPIToken-secret-value" {
		t.Fatalf("reloaded key failed: %v %q", err, got)
	}

	// Key file must be private.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestTamperDetected(t *testing.T) {
	k, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "hrg.key"))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := k.Seal("token")
	if err != nil {
		t.Fatal(err)
	}
	blob[len(blob)-1] ^= 0xff
	if _, err := k.Open(blob); err == nil {
		t.Fatal("tampered ciphertext decrypted without error")
	}
}

func TestWrongKeyFails(t *testing.T) {
	dir := t.TempDir()
	k1, _ := LoadOrCreateKey(filepath.Join(dir, "a.key"))
	k2, _ := LoadOrCreateKey(filepath.Join(dir, "b.key"))
	blob, err := k1.Seal("token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k2.Open(blob); err == nil {
		t.Fatal("wrong key decrypted without error")
	}
}
