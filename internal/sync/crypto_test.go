package sync

import (
	"bytes"
	"errors"
	"testing"
)

func TestCryptoRoundtripEncrypted(t *testing.T) {
	data := []byte(`{"message": "secret conversation with coding agent", "token": "secret_123"}`)
	passphrase := "my-ultra-secure-passphrase-2026"

	encrypted, err := CompressAndEncrypt(data, passphrase)
	if err != nil {
		t.Fatalf("CompressAndEncrypt failed: %v", err)
	}

	if bytes.Equal(encrypted, data) {
		t.Fatal("encrypted data matches plaintext")
	}

	decrypted, err := DecryptAndDecompress(encrypted, passphrase)
	if err != nil {
		t.Fatalf("DecryptAndDecompress failed: %v", err)
	}

	if !bytes.Equal(decrypted, data) {
		t.Fatalf("decrypted data %q != original %q", decrypted, data)
	}
}

func TestCryptoWrongPassphrase(t *testing.T) {
	data := []byte(`sensitive information`)
	passphrase := "correct-passphrase"

	encrypted, err := CompressAndEncrypt(data, passphrase)
	if err != nil {
		t.Fatalf("CompressAndEncrypt failed: %v", err)
	}

	_, err = DecryptAndDecompress(encrypted, "wrong-passphrase")
	if !errors.Is(err, ErrInvalidPassphrase) {
		t.Fatalf("expected ErrInvalidPassphrase, got %v", err)
	}
}

func TestCryptoMissingPassphrase(t *testing.T) {
	data := []byte(`sensitive information`)
	passphrase := "correct-passphrase"

	encrypted, err := CompressAndEncrypt(data, passphrase)
	if err != nil {
		t.Fatalf("CompressAndEncrypt failed: %v", err)
	}

	_, err = DecryptAndDecompress(encrypted, "")
	if !errors.Is(err, ErrPassphraseMissing) {
		t.Fatalf("expected ErrPassphraseMissing, got %v", err)
	}
}

func TestCryptoPlainRoundtrip(t *testing.T) {
	data := []byte(`plain conversation without encryption`)

	blob, err := CompressAndEncrypt(data, "")
	if err != nil {
		t.Fatalf("CompressAndEncrypt plain failed: %v", err)
	}

	decrypted, err := DecryptAndDecompress(blob, "")
	if err != nil {
		t.Fatalf("DecryptAndDecompress plain failed: %v", err)
	}

	if !bytes.Equal(decrypted, data) {
		t.Fatalf("decrypted %q != original %q", decrypted, data)
	}
}
