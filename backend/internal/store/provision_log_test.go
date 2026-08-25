package store

import (
	"context"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

func TestProvisionRunLogAppendPersists(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	pr, err := st.CreateProvisionRun(ctx, &model.ProvisionRun{
		Host: "h", NodeName: "n", Status: "queued", OptionsJSON: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Inspect raw log value after insert (is it '' or NULL?).
	var raw *string
	row := st.db.QueryRowContext(ctx, `SELECT log FROM provision_runs WHERE id = ?`, pr.ID)
	if err := row.Scan(&raw); err != nil {
		t.Fatal("scan:", err)
	}
	if raw == nil {
		t.Fatalf("log is NULL after CreateProvisionRun (DEFAULT not applied) — append will stay NULL forever")
	} else {
		t.Logf("log after insert = %q", *raw)
	}

	// Append and read back.
	if err := st.AppendProvisionRunLog(ctx, pr.ID, "MARKER-1\n"); err != nil {
		t.Fatal("append:", err)
	}
	got, err := st.GetProvisionRun(ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("log after append = %q", got.Log)
	if got.Log != "MARKER-1\n" {
		t.Fatalf("append did not persist: got %q", got.Log)
	}
}
