package dbschema

// These tests need unexported runner injection to exercise a future migration
// without adding production test hooks. Public schema tests live in tests/unit/dbschema.
import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/dellarb/mailmoose/internal/sqlite3driver"
)

func testDB(t *testing.T) (*sql.DB, *sql.Conn) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); db.Close() })
	if err := Migrate(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	return db, conn
}

func TestFutureAdditiveMigrationAndRollback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, expected := testDB(t)
	ddl := `ALTER TABLE accounts ADD COLUMN public_note TEXT NOT NULL DEFAULT '';`
	if _, err := expected.ExecContext(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	hash, err := Fingerprint(ctx, expected)
	if err != nil {
		t.Fatal(err)
	}
	_, actual := testDB(t)
	registered := append(migrations(), migration{"005", ddl, hash})
	if err := migrate(ctx, actual, registered); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, actual, registered); err != nil {
		t.Fatal("restart attempted additive migration again", err)
	}
	if err := Migrate(ctx, actual); err == nil {
		t.Fatal("older binary accepted a newer public database")
	}
	_, failed := testDB(t)
	broken := append(migrations(), migration{"005", ddl + ` SELECT * FROM nonexistent_table;`, hash})
	if err := migrate(ctx, failed, broken); err == nil {
		t.Fatal("failed migration accepted")
	}
	if err := Validate(ctx, failed); err != nil {
		t.Fatal("failed additive migration changed baseline or marker", err)
	}
	broken[4] = migration{"005", ddl, strings.Repeat("0", 64)}
	if err := migrate(ctx, failed, broken); err == nil {
		t.Fatal("incorrect target schema hash accepted")
	}
	if err := Validate(ctx, failed); err != nil {
		t.Fatal("hash validation failure did not roll back DDL and marker", err)
	}
}

func TestSQLFingerprintPreservesSemantics(t *testing.T) {
	t.Parallel()
	base := `CREATE TABLE "test" (a TEXT DEFAULT 'two words', b TEXT CHECK(b<>'--not a comment'));`
	formatted := "/* comment */ CREATE TABLE \"test\" (\n a TEXT DEFAULT 'two words', -- comment\n b TEXT CHECK ( b <> '--not a comment' ) );"
	if sqlTokens(base) != sqlTokens(formatted) {
		t.Fatal("formatting changed fingerprint")
	}
	for _, changed := range []string{
		strings.Replace(base, "two words", "twowords", 1),
		strings.Replace(base, "<>", "=", 1),
		strings.Replace(base, "a TEXT", "aTEXT", 1),
		strings.Replace(base, "TEXT DEFAULT", "TEXT NOT NULL DEFAULT", 1),
	} {
		if sqlTokens(base) == sqlTokens(changed) {
			t.Fatal("semantic change disappeared from fingerprint", changed)
		}
	}
	for _, quoted := range []string{`'it''s a string'`, `"a""b"`, "`a``b`", `[two words]`} {
		if sqlTokens(" "+quoted+" ") != sqlTokens(quoted) {
			t.Fatal("quoted token changed")
		}
	}
}
