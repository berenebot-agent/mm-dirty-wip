// Command prerelease-rebaseline performs the explicit offline pre-beta cutover.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/dellarb/mailmoose/internal/prerelease"
)

func main() {
	dataDir := flag.String("data-dir", "/data", "existing data directory (application must be stopped)")
	check := flag.Bool("check", false, "validate without converting")
	apply := flag.Bool("apply", false, "convert after an offline full-data backup")
	flag.Parse()
	if *check == *apply || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "choose exactly one of --check or --apply; see docs/PRERELEASE_TRANSITION.md")
		os.Exit(2)
	}
	converted, err := prerelease.Run(context.Background(), *dataDir, *apply)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rebaseline:", err)
		os.Exit(1)
	}
	if converted {
		fmt.Println("Converted 080 to public baseline 001. Keep the full backup; never run an old application against this database.")
	} else {
		fmt.Println("Database validated; no changes made.")
	}
}
