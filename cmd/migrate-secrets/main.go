// Command migrate-secrets migrates ciphertext written before stored secrets
// carried an AAD binding, so an instance created before that change keeps
// working. Run it once per instance, with the application stopped:
//
//	# From the directory holding docker-compose.yml and .env
//	docker compose run --rm --user 65532:65532 --entrypoint migrate-secrets \
//	  mailmoose -data-dir /data -dry-run
//	docker compose run --rm --user 65532:65532 --entrypoint migrate-secrets \
//	  mailmoose -data-dir /data
//
// It prints a per-column count and any value it could not convert. It is
// idempotent: a second run reports TOTAL 0. See the upgrade section in
// docs/SELFHOSTING.md.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dellarb/mailmoose/internal/cryptox"
	"github.com/dellarb/mailmoose/internal/migratesecrets"
	_ "github.com/dellarb/mailmoose/internal/sqlite3driver"
)

func main() {
	dataDir := flag.String("data-dir", os.Getenv("DATA_DIR"), "instance data directory (default $DATA_DIR)")
	keyArg := flag.String("app-encryption-key", os.Getenv("APP_ENCRYPTION_KEY"), "instance APP_ENCRYPTION_KEY (default $APP_ENCRYPTION_KEY)")
	dryRun := flag.Bool("dry-run", false, "report what would change without writing")
	flag.Parse()

	if *dataDir == "" {
		fatal("a data directory is required: set DATA_DIR or pass -data-dir")
	}
	if *keyArg == "" {
		fatal("APP_ENCRYPTION_KEY is required: set it in the environment or pass -app-encryption-key")
	}
	dbPath := filepath.Join(*dataDir, "inbox.db")
	if _, err := os.Stat(dbPath); err != nil {
		fatal("cannot read %s: %v", dbPath, err)
	}

	dsn := fmt.Sprintf("file:%s?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		fatal("open database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	rep, err := migratesecrets.Run(ctx, db, *keyArg, *dryRun)
	if err != nil {
		fatal("migration failed: %v", err)
	}

	// Confirm every value now opens through the real production read path.
	key, err := cryptox.DeriveKey(*keyArg)
	if err != nil {
		fatal("derive key: %v", err)
	}
	verified, vFailures, err := migratesecrets.Verify(ctx, db, key, cryptox.DecryptWithAAD, *dryRun)
	if err != nil {
		fatal("verify failed: %v", err)
	}

	if *dryRun {
		fmt.Printf("migrate-secrets: %s (DRY RUN, nothing written)\n", dbPath)
	} else {
		fmt.Printf("migrate-secrets: %s\n", dbPath)
	}
	for _, label := range migratesecrets.Labels() {
		if n := rep.Migrated[label]; n > 0 {
			fmt.Printf("  %-48s %d\n", label, n)
		}
	}
	fmt.Printf("  %-48s %d\n", "TOTAL", rep.Total())

	allFailures := append(append([]string{}, rep.Failures...), vFailures...)
	if *dryRun {
		fmt.Printf("\n%d value(s) already AAD-bound and verified through the production read path.\n", verified)
	} else {
		fmt.Printf("\n%d value(s) verified readable through the production read path.\n", verified)
	}
	if len(allFailures) > 0 {
		fmt.Printf("\n%d value(s) could NOT be read after migration:\n", len(allFailures))
		for _, f := range allFailures {
			fmt.Printf("  %s\n", f)
		}
		fmt.Println("\nThose credentials are sealed under a different APP_ENCRYPTION_KEY.")
		fmt.Println("Re-enter them through the UI; the rest are already migrated.")
		os.Exit(2)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "migrate-secrets: "+format+"\n", args...)
	os.Exit(1)
}
