package sync

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Vault format v2.
//
// v1 derived every vault's key with PBKDF2 and one constant salt shared by
// all agyh users, so a single precomputation could target every vault. v2
// gives each vault its own random salt and uses Argon2id (memory-hard). The
// salt and KDF parameters live in a plaintext header object (vault.json):
// they are not secret, and the key is needed to read anything else.
//
// Blobs carry their format in their magic, so a vault can hold v1 and v2
// blobs at the same time: old blobs stay readable, new writes are v2, and
// `agyh sync migrate` re-encrypts the rest.

var magicEncryptedV2 = []byte("AGY2") // magic (4) + nonce (12) + AES-256-GCM ciphertext/tag, Argon2id key

// HeaderFormat is the vault header version written by this agyh.
const HeaderFormat = 2

// kdfParams are the Argon2id cost parameters, stored in the header so they
// can be raised later without breaking existing vaults.
type kdfParams struct {
	Time      uint32 `json:"time"`
	MemoryKiB uint32 `json:"memory_kib"`
	Threads   uint8  `json:"threads"`
}

var (
	// RFC 9106 "second recommended option": t=3, m=64 MiB.
	defaultKDF = kdfParams{Time: 3, MemoryKiB: 64 << 10, Threads: 4}
	// A header below these is rejected, so whoever can write to the bucket
	// cannot downgrade the KDF for future writes (OWASP minimum: t=2, m=19 MiB).
	minKDF = kdfParams{Time: 2, MemoryKiB: 19 << 10, Threads: 1}
	// Upper bounds keep a hostile header from exhausting memory or CPU.
	maxKDF = kdfParams{Time: 10, MemoryKiB: 1 << 20, Threads: 16}
)

const saltSize = 16

// vaultHeader is stored unencrypted at <prefix>/vault.json.
type vaultHeader struct {
	Format int       `json:"format"`
	KDF    string    `json:"kdf"`
	Salt   []byte    `json:"salt"`
	Params kdfParams `json:"params"`
	// Check is an AES-GCM seal of a constant under the derived key, with the
	// KDF settings as associated data. It detects a wrong passphrase, and a
	// header whose salt or parameters were altered, before anything is written.
	Check []byte `json:"check,omitempty"`
	// LegacyBlobs is set when the header was added to a v1 vault, until
	// `agyh sync migrate` has re-encrypted every v1 blob.
	LegacyBlobs bool `json:"legacy_blobs,omitempty"`
}

var (
	ErrVaultHeader = errors.New("invalid vault header")
	errNoVaultKey  = errors.New("vault has no v2 header yet: run `agyh sync` once to create it")
	checkPlaintext = []byte("agyh vault key check v2")
)

func newVaultHeader(passphrase string, legacy bool) (*vaultHeader, error) {
	h := &vaultHeader{Format: HeaderFormat, KDF: "argon2id", Salt: make([]byte, saltSize), Params: defaultKDF, LegacyBlobs: legacy}
	if _, err := io.ReadFull(rand.Reader, h.Salt); err != nil {
		return nil, fmt.Errorf("generate salt: %w", err)
	}
	if passphrase != "" {
		key := h.derive(passphrase)
		check, err := seal(key, checkPlaintext, h.aad())
		if err != nil {
			return nil, err
		}
		h.Check = check
	}
	return h, nil
}

func (h *vaultHeader) validate() error {
	p := h.Params
	switch {
	case h.Format != HeaderFormat:
		return fmt.Errorf("%w: unsupported format %d (upgrade agyh)", ErrVaultHeader, h.Format)
	case h.KDF != "argon2id":
		return fmt.Errorf("%w: unsupported kdf %q", ErrVaultHeader, h.KDF)
	case len(h.Salt) < saltSize:
		return fmt.Errorf("%w: salt too short", ErrVaultHeader)
	case p.Time < minKDF.Time || p.MemoryKiB < minKDF.MemoryKiB || p.Threads < minKDF.Threads:
		return fmt.Errorf("%w: kdf parameters below the minimum (t=%d, m=%d KiB, p=%d)", ErrVaultHeader, p.Time, p.MemoryKiB, p.Threads)
	case p.Time > maxKDF.Time || p.MemoryKiB > maxKDF.MemoryKiB || p.Threads > maxKDF.Threads:
		return fmt.Errorf("%w: kdf parameters above the maximum (t=%d, m=%d KiB, p=%d)", ErrVaultHeader, p.Time, p.MemoryKiB, p.Threads)
	}
	return nil
}

// aad binds the check value to every KDF setting.
func (h *vaultHeader) aad() []byte {
	return fmt.Appendf(nil, "agyh-vault|%d|%s|%x|%d|%d|%d", h.Format, h.KDF, h.Salt, h.Params.Time, h.Params.MemoryKiB, h.Params.Threads)
}

// derivedV2 caches Argon2id keys per (passphrase, header settings): deriving
// costs ~100 ms and 64 MiB, once per process instead of once per blob.
var derivedV2 sync.Map

func (h *vaultHeader) derive(passphrase string) []byte {
	cacheKey := passphrase + "\x00" + string(h.aad())
	if k, ok := derivedV2.Load(cacheKey); ok {
		return k.([]byte)
	}
	k := argon2.IDKey([]byte(passphrase), h.Salt, h.Params.Time, h.Params.MemoryKiB, h.Params.Threads, 32)
	derivedV2.Store(cacheKey, k)
	return k
}

// key validates the header, derives the key and verifies it against Check.
func (h *vaultHeader) key(passphrase string) ([]byte, error) {
	if err := h.validate(); err != nil {
		return nil, err
	}
	if passphrase == "" {
		return nil, nil // plain (unencrypted) vault mode
	}
	if len(h.Check) == 0 {
		return nil, fmt.Errorf("%w: missing key check (created without a passphrase?)", ErrVaultHeader)
	}
	key := h.derive(passphrase)
	got, err := open(key, h.Check, h.aad())
	if err != nil || !bytes.Equal(got, checkPlaintext) {
		return nil, fmt.Errorf("%w (or the vault header was modified)", ErrInvalidPassphrase)
	}
	return key, nil
}

// vaultKeys encrypts new blobs as v2 and reads v0, v1 and v2 blobs.
type vaultKeys struct {
	passphrase string
	v2         []byte // nil: no header yet (read-only) or plain mode
	hasHeader  bool
}

func (k *vaultKeys) seal(data []byte) ([]byte, error) {
	compressed, err := gzipBytes(data)
	if err != nil {
		return nil, err
	}
	if k.passphrase == "" {
		return append(append([]byte(nil), magicPlain...), compressed...), nil
	}
	if !k.hasHeader || k.v2 == nil {
		return nil, errNoVaultKey
	}
	sealed, err := seal(k.v2, compressed, magicEncryptedV2)
	if err != nil {
		return nil, err
	}
	return append(append([]byte(nil), magicEncryptedV2...), sealed...), nil
}

func (k *vaultKeys) open(blob []byte) ([]byte, error) {
	if !bytes.HasPrefix(blob, magicEncryptedV2) {
		return DecryptAndDecompress(blob, k.passphrase) // v1, plain, legacy gzip
	}
	if k.passphrase == "" {
		return nil, ErrPassphraseMissing
	}
	if k.v2 == nil {
		return nil, fmt.Errorf("%w: blob is v2 but the vault header is missing", ErrVaultHeader)
	}
	compressed, err := open(k.v2, blob[len(magicEncryptedV2):], magicEncryptedV2)
	if err != nil {
		return nil, ErrInvalidPassphrase
	}
	return gunzipBytes(compressed)
}

// seal returns nonce || AES-256-GCM(plain, aad).
func seal(key, plain, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize(), gcm.NonceSize()+len(plain)+gcm.Overhead())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plain, aad), nil
}

func open(key, sealed, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, ErrInvalidPassphrase
	}
	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], aad)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	return cipher.NewGCM(block)
}
