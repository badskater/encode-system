package store

// Regression for the shares-key cipher first-use race (quality review Q1):
// shareCipher() used an unsynchronized check-then-act on s.aead plus a
// non-atomic os.WriteFile of a freshly generated key. Concurrent first use
// (two goroutines generating DIFFERENT keys, last write winning) left stored
// ciphertext permanently undecryptable, and a read racing the write saw a
// partial key ("invalid key size 0"). P3 routes the hot GetSettings /
// SaveSettings paths through this cipher, so the race corrupts notification
// secrets, not just share passwords.
//
// The fix must serialize lazy init AND create the key file atomically
// (never overwriting an existing key — another process may have won).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// raceBaseSettings is a minimal valid settings row for the race tests.
func raceBaseSettings() *model.Settings {
	return &model.Settings{
		ScriptsRoot: "/s", ReleaseRoot: "/r",
		NodeBinDir: `C:\bin`, NodeScriptsDir: `C:\s`, NodeReleaseDir: `C:\r`,
		ScanIntervalSeconds: 30, TasksBeforeReboot: 10, Group: "G", Tag: "T",
	}
}

// settingsSecretsRaceContract drives N concurrent secret-bearing SaveSettings
// on a FRESH store (fresh key file) per iteration, then proves the stored row
// decrypts — both immediately and after a reopen (durability). freshStore
// must return a store whose shares key does not exist yet.
func settingsSecretsRaceContract(t *testing.T, freshStore func(t *testing.T) *Store, reopen func(t *testing.T, s *Store) *Store) {
	t.Helper()
	ctx := context.Background()
	const iterations = 20
	const workers = 8
	for iter := 0; iter < iterations; iter++ {
		s := freshStore(t)
		errs := make([]error, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				st := raceBaseSettings()
				st.NtfyToken = fmt.Sprintf("tok-%d-%d", iter, i)
				st.SMTPPassword = fmt.Sprintf("pw-%d-%d", iter, i)
				errs[i] = s.SaveSettings(ctx, st)
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("iter %d: concurrent SaveSettings %d failed: %v", iter, i, err)
			}
		}
		// The winning row must decrypt, and its token/password must be a
		// MATCHING pair from one goroutine (a cipher/key mismatch surfaces
		// here as a decrypt error or a crossed pair).
		got, err := s.GetSettings(ctx)
		if err != nil {
			t.Fatalf("iter %d: stored settings undecryptable after concurrent first use: %v", iter, err)
		}
		if !strings.HasPrefix(got.NtfyToken, fmt.Sprintf("tok-%d-", iter)) {
			t.Fatalf("iter %d: unexpected token %q", iter, got.NtfyToken)
		}
		wantPW := strings.Replace(got.NtfyToken, "tok-", "pw-", 1)
		if got.SMTPPassword != wantPW {
			t.Fatalf("iter %d: crossed secret pair: token %q password %q", iter, got.NtfyToken, got.SMTPPassword)
		}
		// Durability: a fresh handle (controller restart) must decrypt the
		// same row — the on-disk key must match the cipher that sealed it.
		if reopen != nil {
			s2 := reopen(t, s)
			got2, err := s2.GetSettings(ctx)
			if err != nil {
				t.Fatalf("iter %d: stored settings undecryptable after reopen: %v", iter, err)
			}
			if got2.NtfyToken != got.NtfyToken || got2.SMTPPassword != got.SMTPPassword {
				t.Fatalf("iter %d: secrets changed across reopen", iter)
			}
			s2.Close()
		}
		s.Close()
	}
}

// sharesRaceContract drives concurrent first-use through the SHARES path:
// N concurrent CreateShare calls with distinct passwords on a fresh store;
// every stored password must decrypt back to its original value.
func sharesRaceContract(t *testing.T, freshStore func(t *testing.T) *Store) {
	t.Helper()
	ctx := context.Background()
	s := freshStore(t)
	defer s.Close()
	const workers = 8
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.CreateShare(ctx, &model.Share{
				Name: fmt.Sprintf("race-share-%d", i), Kind: model.ShareSMB, Role: model.ShareRoleScripts,
				Server: "nas", Path: "/export", Username: "u", Password: fmt.Sprintf("share-pw-%d", i),
				Enabled: true,
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent CreateShare %d failed: %v", i, err)
		}
	}
	shares, err := s.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares after concurrent first use: %v", err)
	}
	if len(shares) != workers {
		t.Fatalf("want %d shares, got %d", workers, len(shares))
	}
	for _, sh := range shares {
		want := strings.Replace(sh.Name, "race-share-", "share-pw-", 1)
		if sh.Password != want {
			t.Fatalf("share %q password = %q, want %q (key/cipher mismatch)", sh.Name, sh.Password, want)
		}
	}
}

// TestShareCipherConcurrentFirstUseSettings is the SQLite regression: 20
// iterations of 8 concurrent secret-bearing SaveSettings on a fresh store,
// then every stored secret decrypts (also across a reopen).
func TestShareCipherConcurrentFirstUseSettings(t *testing.T) {
	settingsSecretsRaceContract(t,
		func(t *testing.T) *Store {
			s, err := Open(filepath.Join(t.TempDir(), "race.db"))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			return s
		},
		func(t *testing.T, s *Store) *Store {
			s2, err := Open(s.dbPath)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			return s2
		})
}

// TestShareCipherConcurrentFirstUseShares is the SQLite shares-path variant.
func TestShareCipherConcurrentFirstUseShares(t *testing.T) {
	sharesRaceContract(t, func(t *testing.T) *Store {
		s, err := Open(filepath.Join(t.TempDir(), "race.db"))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	})
}

// TestShareCipherKeyFileNeverOverwritten: an existing key file must survive
// concurrent first use untouched — a second Store handle opening the same DB
// must reuse the key, not regenerate it (regeneration bricks every secret
// sealed with the original key).
func TestShareCipherKeyFileNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s1, err := Open(filepath.Join(dir, "race.db"))
	if err != nil {
		t.Fatal(err)
	}
	st := raceBaseSettings()
	st.NtfyToken = "first-" + "writer-tok"
	if err := s1.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	keyPath := s1.shareKeyPath()
	before, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 32 {
		t.Fatalf("key file size = %d, want 32", len(before))
	}
	// A second handle (separate process equivalent) must adopt the same key.
	s2, err := Open(filepath.Join(dir, "race.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	st2 := raceBaseSettings()
	st2.SMTPPassword = "second-" + "writer-pw"
	if err := s2.SaveSettings(ctx, st2); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("existing shares key file was overwritten by a second handle")
	}
	got, err := s1.GetSettings(ctx)
	if err != nil {
		t.Fatalf("first handle can no longer decrypt: %v", err)
	}
	if got.SMTPPassword != st2.SMTPPassword {
		t.Fatalf("second writer secret lost: %q", got.SMTPPassword)
	}
	s1.Close()
}

// TestShareCipherConcurrentFirstUseContractPG runs the same race contract on
// Postgres (env-gated). Each iteration gets a fresh key file via
// ENCODE_SHARES_KEY_FILE and a fresh Store handle so first-use is exercised
// every time; the settings/shares tables are truncated between iterations.
func TestShareCipherConcurrentFirstUseContractPG(t *testing.T) {
	dsn := os.Getenv("ENCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("ENCODE_TEST_PG_DSN not set; skipping PG cipher race contract")
	}
	freshStore := func(t *testing.T) *Store {
		t.Helper()
		t.Setenv("ENCODE_SHARES_KEY_FILE", filepath.Join(t.TempDir(), "race.shares-key"))
		s, err := Open(dsn)
		if err != nil {
			t.Fatalf("open pg: %v", err)
		}
		ctx := context.Background()
		for _, tbl := range []string{"settings", "shares"} {
			if _, err := s.db.ExecContext(ctx, "TRUNCATE TABLE "+tbl+" RESTART IDENTITY CASCADE"); err != nil {
				t.Fatalf("truncate %s: %v", tbl, err)
			}
		}
		return s
	}
	settingsSecretsRaceContract(t, freshStore, nil)
	sharesRaceContract(t, freshStore)
}
