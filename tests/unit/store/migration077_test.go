package store_test

import (
	"context"
	"testing"

	"github.com/dellarb/mailmoose/internal/store"
)

// TestMigration077PreSendMarker proves migration 077 adds the durable pre-send
// marker column to both outbound queues, with an empty default (so every row
// pre-dating the migration is "no send in flight").
func TestMigration077PreSendMarker(t *testing.T) {
	t.Parallel()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n := markerCount(t, st, "077"); n != 1 {
		t.Fatalf("migration 077 marker count %d", n)
	}
	db := rawDB(t, st.Path())
	defer db.Close()
	for _, tc := range [][2]string{{"messages", "send_attempt_state"}, {"outbound_workflow", "send_attempt_state"}} {
		var n int
		if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, tc[0], tc[1]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("column %s.%s missing after migration 077", tc[0], tc[1])
		}
	}
}
