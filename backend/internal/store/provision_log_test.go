package store

import (
	"context"
	"strings"
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
	if err := st.AppendProvisionRunLog(ctx, pr.ID, "MARKER-1\n", 0); err != nil {
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

	// Cap enforcement: the persisted log must stay bounded (tail kept).
	long := ""
	for i := 0; i < 100; i++ {
		long += "0123456789" // 1 KB total
	}
	for i := 0; i < 10; i++ { // push 10 KB through a 2 KB cap
		if err := st.AppendProvisionRunLog(ctx, pr.ID, long, 2048); err != nil {
			t.Fatal("capped append:", err)
		}
	}
	got, err = st.GetProvisionRun(ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Log) > 2048 {
		t.Fatalf("persisted log exceeds cap: %d bytes", len(got.Log))
	}
	if !strings.HasSuffix(got.Log, "9") {
		t.Fatalf("cap must keep the TAIL, got suffix %q", got.Log[len(got.Log)-4:])
	}
}
