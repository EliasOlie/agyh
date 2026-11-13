package sync

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/pbkdf2"
)

var (
	magicEncrypted = []byte("AGY1") // AGY encrypted v1: magic (4) + nonce (12) + AES-256-GCM tag/ciphertext
	magicPlain     = []byte("AGY0") // AGY compressed plain v1: magic (4) + gzip payload

	pbkdf2Salt = []byte("agyh-vault-v1-kdf-salt")
	pbkdf2Iter = 100_000
)

var (
	ErrInvalidPassphrase = errors.New("invalid passphrase or corrupted vault data")
	ErrPassphraseMissing = errors.New("vault data is encrypted: passphrase is required")
)

func deriveKey(passphrase string) []byte {
	return pbkdf2.Key([]byte(passphrase), pbkdf2Salt, pbkdf2Iter, 32, sha256.New)
}

// CompressAndEncrypt compresses payload with gzip and, if passphrase is not empty,
// encrypts it using AES-256-GCM with a PBKDF2-derived key.
func CompressAndEncrypt(data []byte, passphrase string) ([]byte, error) {
	var gzBuf bytes.Buffer
	gw, err := gzip.NewWriterLevel(&gzBuf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := gw.Write(data); err != nil {
		_ = gw.Close()
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	compressed := gzBuf.Bytes()

	if passphrase == "" {
		out := make([]byte, len(magicPlain)+len(compressed))
		copy(out, magicPlain)
		copy(out[len(magicPlain):], compressed)
		return out, nil
	}

	key := deriveKey(passphrase)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	sealed := gcm.Seal(nil, nonce, compressed, nil)

	out := make([]byte, len(magicEncrypted)+len(nonce)+len(sealed))
	copy(out, magicEncrypted)
	copy(out[4:], nonce)
	copy(out[4+len(nonce):], sealed)
	return out, nil
}

// DecryptAndDecompress decrypts (if encrypted) and decompresses a vault blob.
func DecryptAndDecompress(blob []byte, passphrase string) ([]byte, error) {
	if len(blob) < 4 {
		return nil, errors.New("blob too small to be a valid agyh vault artifact")
	}

	var compressed []byte
	switch {
	case bytes.Equal(blob[:4], magicEncrypted):
		if passphrase == "" {
			return nil, ErrPassphraseMissing
		}
		key := deriveKey(passphrase)
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		nonceSize := gcm.NonceSize()
		if len(blob) < 4+nonceSize {
			return nil, ErrInvalidPassphrase
		}
		nonce := blob[4 : 4+nonceSize]
		ciphertext := blob[4+nonceSize:]
		plain, err := gcm.Open(nil, nonce, ciphertext, nil)
		if err != nil {
			return nil, ErrInvalidPassphrase
		}
		compressed = plain

	case bytes.Equal(blob[:4], magicPlain):
		compressed = blob[4:]

	default:
		// Attempt fallback as raw gzip if legacy or unversioned
		compressed = blob
	}

	gr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("decompress gzip: %w", err)
	}
	defer gr.Close()

	out, err := io.ReadAll(gr)
	if err != nil {
		return nil, fmt.Errorf("read decompressed data: %w", err)
	}
	return out, nil
}
