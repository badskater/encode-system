package store

// Regression for P4 review Q9: shareCipher caches s.aead for the Store
// lifetime, so an in-place PG restore that swaps the shares-key file left
// restored secrets undecryptable until a restart. InvalidateShareCipher
// drops the cache so the next use re-reads the key file.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInvalidateShareCipherRereadsKey(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	// First use creates the key and caches the cipher (key A).
	ct1, err := s.encryptSharePassword("alpha")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := s.SharesKeyPath()
	keyA, err := os.ReadFile(keyPath)
	if err != nil || len(keyA) != 32 {
		t.Fatalf("key A: %v len=%d", err, len(keyA))
	}

	// Swap in a DIFFERENT key (key B) — simulates placing the restored
	// bundle's key file beside the DB.
	keyB := make([]byte, 32)
	for i := range keyB {
		keyB[i] = byte(i + 1)
	}
	if err := os.WriteFile(keyPath, keyB, 0o600); err != nil {
		t.Fatal(err)
	}

	// WITHOUT invalidation the cached cipher (key A) is still used: a new
	// ciphertext is sealed with A, not the on-disk B. This is the bug.
	ct2, err := s.encryptSharePassword("beta")
	if err != nil {
		t.Fatal(err)
	}

	// Invalidate: the next use must re-read key B from disk.
	s.InvalidateShareCipher()
	if _, err := s.decryptSharePassword(ct2); err == nil {
		t.Fatal("ct2 was sealed with cached key A — after invalidation it must NOT decrypt with key B")
	}
	ct3, err := s.encryptSharePassword("gamma")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.decryptSharePassword(ct3)
	if err != nil || got != "gamma" {
		t.Fatalf("post-invalidation cipher must use key B: %q %v", got, err)
	}
	// Sanity: ct1 (sealed with A) is undecryptable now — expected, the key
	// really changed; the point is the cipher FOLLOWED the file.
	if _, err := s.decryptSharePassword(ct1); err == nil {
		t.Fatal("ct1 sealed with key A must not decrypt under key B")
	}
	// The key file itself was never rewritten by the cipher path.
	onDisk, _ := os.ReadFile(keyPath)
	if string(onDisk) != string(keyB) {
		t.Fatal("invalidation must re-read, never regenerate, the key file")
	}
}
