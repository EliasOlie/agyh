package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	gosync "sync"
	"testing"

	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/model"
	"github.com/EliasOlie/agyh/internal/store"
)

const headerObj = "vault/vault.json"

func readHeader(t *testing.T, v *vault) vaultHeader {
	t.Helper()
	raw := v.get(headerObj)
	if raw == nil {
		t.Fatal("vault has no header")
	}
	var h vaultHeader
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatal(err)
	}
	return h
}

func writeHeader(t *testing.T, v *vault, h vaultHeader) {
	t.Helper()
	raw, _ := json.Marshal(h)
	v.set(headerObj, raw)
}

// blobFormats counts stored blobs (manifest + sessions) by magic. Kit blobs
// are covered by kit_test.go.
func blobFormats(v *vault) map[string]int {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := map[string]int{}
	for k, b := range v.objects {
		if strings.HasSuffix(k, ".blob") && !strings.Contains(k, "/kit/") && len(b) >= 4 {
			out[string(b[:4])]++
		}
	}
	return out
}

// writeV1Vault stores sessions exactly like agyh <= v0.2 did: v1 blobs and manifest, no header.
func writeV1Vault(t *testing.T, v *vault, passphrase string, sessions ...model.Session) {
	t.Helper()
	m := NewManifest()
	for _, s := range sessions {
		raw, _ := json.Marshal(Bundle{Session: s, Messages: []model.Message{{Role: "user", Text: s.Title + " legado"}}})
		blob, err := CompressAndEncrypt(raw, passphrase)
		if err != nil {
			t.Fatal(err)
		}
		v.set("vault/sessions/"+s.Agent+"/"+s.ID+".blob", blob)
		m.Sessions[s.Key()] = summaryOf(s, "")
	}
	raw, _ := json.Marshal(m)
	blob, _ := CompressAndEncrypt(raw, passphrase)
	v.set("vault/manifest.blob", blob)
}

func TestNewVaultUsesPerVaultSaltAndV2Blobs(t *testing.T) {
	v1, cfg1 := newVault(t)
	v2, cfg2 := newVault(t)
	for _, c := range []struct {
		v   *vault
		cfg config.Sync
	}{{v1, cfg1}, {v2, cfg2}} {
		a := newMachine(t, c.cfg)
		a.add(t, testSession(1), testSession(2))
		if res := a.sync(t); res.LegacyBlobs {
			t.Fatalf("fresh vault reported legacy blobs: %+v", res)
		}
		if f := blobFormats(c.v); f["AGY2"] != 3 || len(f) != 1 {
			t.Fatalf("every blob must be v2: %v", f)
		}
		if st := a.status(t); st.LegacyEncryption {
			t.Fatalf("status: %+v", st)
		}
	}
	h1, h2 := readHeader(t, v1), readHeader(t, v2)
	if h1.KDF != "argon2id" || len(h1.Salt) != saltSize || h1.Params != defaultKDF || h1.LegacyBlobs || len(h1.Check) == 0 {
		t.Fatalf("header: %+v", h1)
	}
	if bytes.Equal(h1.Salt, h2.Salt) || bytes.Equal(h1.derive(cfg1.Passphrase), h2.derive(cfg2.Passphrase)) {
		t.Fatal("two vaults with the same passphrase share a salt or key")
	}
	if bytes.Contains(v1.get(headerObj), []byte(cfg1.Passphrase)) {
		t.Fatal("passphrase leaked into the plaintext header")
	}

	// A second machine reads it.
	b := newMachine(t, cfg1)
	if res := b.sync(t); res.Downloaded != 2 {
		t.Fatalf("second machine: %+v", res)
	}
}

func TestWrongPassphraseFailsBeforeAnyWrite(t *testing.T) {
	v, cfg := newVault(t)
	a := newMachine(t, cfg)
	a.add(t, testSession(1))
	a.sync(t)
	before := v.allPuts.Load()

	cfg.Passphrase = "wrong passphrase"
	b := newMachine(t, cfg)
	b.add(t, testSession(2))
	if _, err := b.e.Status(context.Background()); !errors.Is(err, ErrInvalidPassphrase) {
		t.Fatalf("status with a wrong passphrase: %v", err)
	}
	if _, err := b.e.Sync(context.Background()); !errors.Is(err, ErrInvalidPassphrase) {
		t.Fatalf("sync with a wrong passphrase: %v", err)
	}
	if v.allPuts.Load() != before {
		t.Fatal("a wrong passphrase wrote to the vault")
	}
}

func TestTamperedHeaderIsRejected(t *testing.T) {
	ctx := context.Background()
	for name, tamper := range map[string]func(h *vaultHeader){
		"swapped salt":       func(h *vaultHeader) { h.Salt = bytes.Repeat([]byte{7}, saltSize) },
		"weaker params":      func(h *vaultHeader) { h.Params.Time = 1 }, // downgrade attempt
		"tiny memory":        func(h *vaultHeader) { h.Params.MemoryKiB = 8 },
		"huge memory":        func(h *vaultHeader) { h.Params.MemoryKiB = 1 << 30 },
		"same-valid params":  func(h *vaultHeader) { h.Params.Time = 4 }, // valid range, but Check no longer matches
		"short salt":         func(h *vaultHeader) { h.Salt = h.Salt[:8] },
		"unknown kdf":        func(h *vaultHeader) { h.KDF = "md5" },
		"future format":      func(h *vaultHeader) { h.Format = 3 },
		"check removed":      func(h *vaultHeader) { h.Check = nil },
		"check from another": func(h *vaultHeader) { h.Check = bytes.Repeat([]byte{1}, 40) },
	} {
		t.Run(name, func(t *testing.T) {
			v, cfg := newVault(t)
			a := newMachine(t, cfg)
			a.add(t, testSession(1))
			a.sync(t)
			h := readHeader(t, v)
			tamper(&h)
			writeHeader(t, v, h)
			before := v.allPuts.Load()

			b := newMachine(t, cfg)
			b.add(t, testSession(2))
			_, err := b.e.Sync(ctx)
			if !errors.Is(err, ErrVaultHeader) && !errors.Is(err, ErrInvalidPassphrase) {
				t.Fatalf("tampered header accepted: %v", err)
			}
			if v.allPuts.Load() != before {
				t.Fatal("wrote to a vault with a tampered header")
			}
		})
	}
}

func TestLegacyVaultUpgradeThenMigrate(t *testing.T) {
	v, cfg := newVault(t)
	ctx := context.Background()
	old := []model.Session{testSession(1), testSession(2), testSession(3)}
	writeV1Vault(t, v, cfg.Passphrase, old...)

	b := newMachine(t, cfg)
	if st := b.status(t); !st.LegacyEncryption || st.ToDownload != 3 {
		t.Fatalf("status of a v1 vault: %+v", st)
	}
	if v.get(headerObj) != nil {
		t.Fatal("a read-only command created the header")
	}

	// A new session synced from an upgraded machine: header appears, new data is v2,
	// old blobs stay v1 and readable.
	b.add(t, testSession(4))
	res := b.sync(t)
	if res.Downloaded != 3 || res.Uploaded != 1 || !res.LegacyBlobs {
		t.Fatalf("first upgraded sync: %+v", res)
	}
	if h := readHeader(t, v); !h.LegacyBlobs {
		t.Fatal("header added to a v1 vault must be flagged legacy")
	}
	if f := blobFormats(v); f["AGY1"] != 3 || f["AGY2"] != 2 {
		t.Fatalf("mixed vault: %v", f)
	}
	c := newMachine(t, cfg)
	if r, err := c.e.Restore(ctx); err != nil || r.Downloaded != 4 {
		t.Fatalf("restore from a mixed vault: %+v %v", r, err)
	}

	r, err := b.e.Migrate(ctx)
	if err != nil || r.Uploaded != 3 || r.Total != 4 {
		t.Fatalf("migrate: %+v %v", r, err)
	}
	if f := blobFormats(v); f["AGY1"] != 0 || f["AGY2"] != 5 {
		t.Fatalf("after migrate: %v", f)
	}
	if readHeader(t, v).LegacyBlobs || b.status(t).LegacyEncryption {
		t.Fatal("legacy flag not cleared")
	}
	if r, err := b.e.Migrate(ctx); err != nil || r.Uploaded != 0 {
		t.Fatalf("second migrate must be a no-op: %+v %v", r, err)
	}

	d := newMachine(t, cfg)
	if r, err := d.e.Restore(ctx); err != nil || r.Downloaded != 4 {
		t.Fatalf("restore after migrate: %+v %v", r, err)
	}
	if hits, _ := d.st.Search(ctx, "legado", store.Filter{}); len(hits) != 3 {
		t.Fatalf("migrated transcripts not readable: %d hits", len(hits))
	}
}

func TestWrongPassphraseNeverCreatesHeaderOnLegacyVault(t *testing.T) {
	v, cfg := newVault(t)
	writeV1Vault(t, v, cfg.Passphrase, testSession(1))
	cfg.Passphrase = "not the vault passphrase"
	b := newMachine(t, cfg)
	b.add(t, testSession(2))
	if _, err := b.e.Sync(context.Background()); !errors.Is(err, ErrInvalidPassphrase) {
		t.Fatalf("sync: %v", err)
	}
	if _, err := b.e.Migrate(context.Background()); !errors.Is(err, ErrInvalidPassphrase) {
		t.Fatalf("migrate: %v", err)
	}
	if v.get(headerObj) != nil {
		t.Fatal("a wrong passphrase created the header: the real owner would be locked out")
	}
}

func TestConcurrentHeaderCreationAgreesOnOneSalt(t *testing.T) {
	_, cfg := newVault(t)
	const n = 5
	keys := make([][]byte, n)
	var wg gosync.WaitGroup
	for i := range n {
		m := newMachine(t, cfg)
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, err := m.e.ready(context.Background(), true)
			if err != nil {
				t.Error(err)
				return
			}
			keys[i] = k.v2
		}()
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if !bytes.Equal(keys[0], keys[i]) {
			t.Fatal("machines racing to create the header ended up with different keys")
		}
	}
}

func TestPutIfAbsent(t *testing.T) {
	v, cfg := newVault(t)
	e := newMachine(t, cfg).e
	ctx := context.Background()
	if ok, err := e.s3.PutIfAbsent(ctx, "k", []byte("first")); !ok || err != nil {
		t.Fatalf("first: %v %v", ok, err)
	}
	if ok, err := e.s3.PutIfAbsent(ctx, "k", []byte("second")); ok || err != nil {
		t.Fatalf("second: %v %v", ok, err)
	}
	if got := string(v.get("k")); got != "first" {
		t.Fatalf("object overwritten: %q", got)
	}
}

func TestVaultKeysWithoutHeader(t *testing.T) {
	k := &vaultKeys{passphrase: "p"}
	if _, err := k.seal([]byte("x")); !errors.Is(err, errNoVaultKey) {
		t.Fatalf("seal without header: %v", err)
	}
	if _, err := k.open([]byte("AGY2garbage-garbage-garbage")); !errors.Is(err, ErrVaultHeader) {
		t.Fatalf("open v2 without header: %v", err)
	}
	v1, _ := CompressAndEncrypt([]byte("legacy"), "p")
	if got, err := k.open(v1); err != nil || string(got) != "legacy" {
		t.Fatalf("v1 must still open: %q %v", got, err)
	}
	// A v2 blob is bound to its magic: altering any byte fails authentication.
	h, _ := newVaultHeader("p", false)
	key, err := h.key("p")
	if err != nil {
		t.Fatal(err)
	}
	k2 := &vaultKeys{passphrase: "p", v2: key, hasHeader: true}
	blob, _ := k2.seal([]byte("secret"))
	blob[len(blob)-1] ^= 1
	if _, err := k2.open(blob); !errors.Is(err, ErrInvalidPassphrase) {
		t.Fatalf("tampered blob: %v", err)
	}
}

// header.key must reject tampering on its own, without relying on the manifest
// failing to decrypt (an empty vault has no manifest to catch it).
func TestHeaderKeyRejectsTamperingDirectly(t *testing.T) {
	good, err := newVaultHeader("pass", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := good.key("pass"); err != nil {
		t.Fatalf("valid header: %v", err)
	}
	if _, err := good.key("other"); !errors.Is(err, ErrInvalidPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	for name, tamper := range map[string]func(h *vaultHeader){
		"salt":                 func(h *vaultHeader) { h.Salt = bytes.Repeat([]byte{9}, saltSize) },
		"valid but different":  func(h *vaultHeader) { h.Params.Time++ },
		"below minimum time":   func(h *vaultHeader) { h.Params.Time = minKDF.Time - 1 },
		"below minimum memory": func(h *vaultHeader) { h.Params.MemoryKiB = minKDF.MemoryKiB - 1 },
		"above maximum memory": func(h *vaultHeader) { h.Params.MemoryKiB = maxKDF.MemoryKiB + 1 },
		"too many threads":     func(h *vaultHeader) { h.Params.Threads = maxKDF.Threads + 1 },
		"no check":             func(h *vaultHeader) { h.Check = nil },
	} {
		h := *good
		h.Salt = append([]byte(nil), good.Salt...)
		tamper(&h)
		if _, err := h.key("pass"); err == nil {
			t.Errorf("%s: tampered header accepted", name)
		}
	}
}
