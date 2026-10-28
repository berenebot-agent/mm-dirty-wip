// oneprobe is manual, developer-owned compatibility tooling. Never run in CI
// with live credentials; it is not included in the production image.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/dellarb/mammothmail/internal/oneprobe"
)

func main() {
	drafts := flag.Bool("drafts", false, "create/update/delete only a probe-owned disposable draft")
	send := flag.Bool("send-approved", false, "send one approved test message to ONE_PROBE_SEND_TO (requires -drafts)")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	report, err := oneprobe.Run(ctx, oneprobe.Options{
		APIKey: os.Getenv("ONE_API_KEY"), ConnectionID: os.Getenv("ONE_CONNECTION_ID"),
		WriteDrafts: *drafts, SendApproved: *send, SendTo: os.Getenv("ONE_PROBE_SEND_TO"),
	})
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if e := enc.Encode(report); e != nil {
		fmt.Fprintln(os.Stderr, "could not write safe compatibility report")
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !report.ReleaseGate {
		fmt.Fprintln(os.Stderr, "release gate incomplete: do not advertise a fully functional connector yet")
	}
}
