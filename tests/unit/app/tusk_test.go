package app_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/transport/mxdial"
)

// stubAntler is a deterministic endpoint resolver for save-flow tests. A stub
// with no source answers as the live GitHub manifest did, which is what the
// setup flow reads by default; a test that wants the built-in defaults sets
// source (and err) explicitly.
type stubAntler struct {
	receivers []mxdial.AntlerReceiver
	source    mxdial.AntlerSource
	fetchedAt time.Time
	// fetchErr is carried on a resolved set as the failure that forced the
	// built-in defaults; err, by contrast, means no receiver set was available at
	// all, which is a setup error rather than a fallback.
	fetchErr error
	err      error
	calls    int
}

func (s *stubAntler) Receivers(context.Context) (mxdial.AntlerEndpointSet, error) {
	s.calls++
	if s.err != nil {
		return mxdial.AntlerEndpointSet{}, s.err
	}
	source, at := s.source, s.fetchedAt
	if source == "" {
		source = mxdial.AntlerSourceGitHub
		if at.IsZero() {
			at = time.Date(2026, 10, 18, 5, 12, 3, 0, time.UTC)
		}
	}
	return mxdial.AntlerEndpointSet{Receivers: s.receivers, Source: source, FetchedAt: at, Err: s.fetchErr}, nil
}

func antlerReceivers() []mxdial.AntlerReceiver {
	return []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
		{ID: "antler-2", SessionURL: "https://tusk2.example.test", SMTPHostname: "tusk2.example.test", MXPriority: 20},
	}
}

// TestDialMXAntlerSetupSnapshotsEndpoints proves a Tusk MX setup validates
// the contact email, resolves the live manifest, snapshots both receiver URLs
// and hostnames, and assigns a stable setup id that a resave retains.
func TestDialMXAntlerSetupSnapshotsEndpoints(t *testing.T) {
	t.Parallel()
	svc, u, d := newMXServiceWithOutboundPolicy(t, true)
	stub := &stubAntler{receivers: antlerReceivers()}
	svc.AntlerEndpoints = stub
	ctx := context.Background()

	saved, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test", "enforcement": "moderate",
	}, false)
	if err != nil {
		t.Fatalf("antler save: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("manifest fetches = %d, want 1", stub.calls)
	}
	values, err := svc.DecryptDomainReceivingConfig(saved)
	if err != nil {
		t.Fatal(err)
	}
	if values["receiver_urls"] != "https://antler1.example.test,https://tusk2.example.test" {
		t.Fatalf("receiver urls not snapshotted: %v", values["receiver_urls"])
	}
	if values["contact_email"] != "ops@example.test" {
		t.Fatalf("contact email not stored: %v", values["contact_email"])
	}
	setupID, _ := values["setup_id"].(string)
	if setupID == "" {
		t.Fatal("setup id missing")
	}
	receivers := app.AntlerReceiversFromConfig(values)
	if len(receivers) != 2 || receivers[1].MXPriority != 20 {
		t.Fatalf("receiver snapshot missing: %+v", receivers)
	}
	// The snapshot records where it came from, so the status view can state it.
	if values["receiver_manifest_source"] != string(mxdial.AntlerSourceGitHub) {
		t.Fatalf("manifest source = %v, want github", values["receiver_manifest_source"])
	}
	if values["receiver_manifest_at"] != "2026-10-18T05:12:03Z" {
		t.Fatalf("manifest fetch time = %v, want the resolver's fetch time", values["receiver_manifest_at"])
	}

	// A resave with no email keeps the stored contact and the same setup id.
	resaved, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "dialmx", map[string]any{"service": mxdial.ServiceAntler}, false)
	if err != nil {
		t.Fatalf("resave: %v", err)
	}
	again, err := svc.DecryptDomainReceivingConfig(resaved)
	if err != nil {
		t.Fatal(err)
	}
	if again["contact_email"] != "ops@example.test" || again["setup_id"] != setupID {
		t.Fatalf("resave changed registration: %v %v", again["contact_email"], again["setup_id"])
	}
	// The record describes the receivers the published MX records point at, so a
	// re-save keeps it rather than re-stamping it as a fresh fetch.
	if again["receiver_manifest_source"] != values["receiver_manifest_source"] || again["receiver_manifest_at"] != values["receiver_manifest_at"] {
		t.Fatalf("resave changed the manifest record: %v %v", again["receiver_manifest_source"], again["receiver_manifest_at"])
	}
	if stub.calls != 1 {
		t.Fatalf("resave re-resolved the manifest: %d fetches", stub.calls)
	}
}

// TestDialMXAntlerRecordsAndLogsTheManifestSource proves the two surfaces the
// operator reads: the container log at setup save, and the recorded source the
// status dialog states. A live fetch logs the GitHub URL and the fetch time; a
// fetch that fell back to the built-in defaults logs the failure and records no
// fetch time, so the dialog says the defaults were used.
func TestDialMXAntlerRecordsAndLogsTheManifestSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	svc, u, d := newMXServiceWithOutboundPolicy(t, true)
	sink := newLogSink()
	svc.Log = slog.New(sink)
	svc.AntlerEndpoints = &stubAntler{receivers: antlerReceivers()}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	msgs := sink.messages()
	if len(msgs) != 1 || msgs[0] != "Tusk receivers updated from GitHub" {
		t.Fatalf("setup log messages = %q, want the live-fetch line", msgs)
	}
	if got := sink.values("url"); len(got) != 1 || got[0] != mxdial.AntlerManifestURL {
		t.Fatalf("logged manifest url = %q, want %q", got, mxdial.AntlerManifestURL)
	}
	if got := sink.values("receivers"); len(got) != 1 || got[0] != "2" {
		t.Fatalf("logged receiver count = %q, want 2", got)
	}

	// A domain whose fetch fell back to the embedded copy: the log names the
	// failure, and the record carries no fetch time.
	svc2, u2, d2 := newMXServiceWithOutboundPolicy(t, true)
	sink2 := newLogSink()
	svc2.Log = slog.New(sink2)
	svc2.AntlerEndpoints = &stubAntler{
		receivers: antlerReceivers(),
		source:    mxdial.AntlerSourceEmbedded,
		fetchErr:  errors.New("network down"),
	}
	fallback, _, err := svc2.SaveDomainReceivingConfig(ctx, u2.AccountID, d2.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false)
	if err != nil {
		t.Fatalf("fallback save: %v", err)
	}
	msgs = sink2.messages()
	if len(msgs) != 1 || msgs[0] != "could not fetch Tusk receivers from GitHub; using built-in defaults" {
		t.Fatalf("fallback log messages = %q, want the built-in-defaults line", msgs)
	}
	if got := sink2.values("error"); len(got) != 1 || !strings.Contains(got[0], "network down") {
		t.Fatalf("fallback log error = %q, want the fetch failure", got)
	}
	values, err := svc2.DecryptDomainReceivingConfig(fallback)
	if err != nil {
		t.Fatal(err)
	}
	if values["receiver_manifest_source"] != string(mxdial.AntlerSourceEmbedded) {
		t.Fatalf("fallback manifest source = %v, want embedded", values["receiver_manifest_source"])
	}
	if _, ok := values["receiver_manifest_at"]; ok {
		t.Fatalf("a fallback recorded a fetch time: %v", values["receiver_manifest_at"])
	}
	if app.AntlerReceiversFromConfig(values) == nil {
		t.Fatal("fallback snapshot missing its receivers")
	}
}

// TestDialMXAntlerResaveKeepsReceiverSnapshot proves an existing setup keeps the
// receiver set its MX records already point at, even when the live manifest has
// changed since. The wizard saves the configuration more than once while an
// operator works through its steps, so a re-save must not silently re-point the
// domain at a different receiver.
func TestDialMXAntlerResaveKeepsReceiverSnapshot(t *testing.T) {
	t.Parallel()
	svc, u, d := newMXServiceWithOutboundPolicy(t, true)
	stub := &stubAntler{receivers: antlerReceivers()}
	svc.AntlerEndpoints = stub
	ctx := context.Background()

	saved, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false)
	if err != nil {
		t.Fatalf("antler save: %v", err)
	}
	original, _ := svc.DecryptDomainReceivingConfig(saved)
	originalURLs := original["receiver_urls"]

	// The live manifest now lists a different receiver set.
	stub.receivers = []mxdial.AntlerReceiver{{ID: "antler-9", SessionURL: "https://antler9.example.test", SMTPHostname: "antler9.example.test", MXPriority: 10}}
	resaved, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "new@example.test", "enforcement": "hard",
	}, false)
	if err != nil {
		t.Fatalf("resave: %v", err)
	}
	again, _ := svc.DecryptDomainReceivingConfig(resaved)
	if again["receiver_urls"] != originalURLs {
		t.Fatalf("resave re-pointed the receiver set: %v -> %v", originalURLs, again["receiver_urls"])
	}
	if again["contact_email"] != "new@example.test" {
		t.Fatalf("resave did not apply the new contact: %v", again["contact_email"])
	}
	// The new setup must not have been created from the changed manifest.
	if strings.Contains(again["receiver_urls"].(string), "antler9") {
		t.Fatalf("resave adopted a changed manifest: %v", again["receiver_urls"])
	}
}

// TestUpdateAntlerSettingsEditsInPlace proves the status view's settings action
// updates the contact email and the enforcement policy while preserving the
// receiver snapshot, setup identity and the domain credential.
func TestUpdateAntlerSettingsEditsInPlace(t *testing.T) {
	t.Parallel()
	svc, u, d := newMXServiceWithOutboundPolicy(t, true)
	stub := &stubAntler{receivers: antlerReceivers()}
	svc.AntlerEndpoints = stub
	ctx := context.Background()

	saved, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false)
	if err != nil {
		t.Fatalf("antler save: %v", err)
	}
	before, _ := svc.DecryptDomainReceivingConfig(saved)

	updated, err := svc.UpdateAntlerSettings(ctx, u.AccountID, d.ID, app.AntlerSettings{
		ContactEmail: "new@example.test", Enforcement: "hard",
	})
	if err != nil {
		t.Fatalf("update settings: %v", err)
	}
	after, _ := svc.DecryptDomainReceivingConfig(updated)
	if after["contact_email"] != "new@example.test" || after["enforcement"] != "hard" {
		t.Fatalf("settings not applied: %v %v", after["contact_email"], after["enforcement"])
	}
	for _, key := range []string{"receiver_urls", "setup_id", "antler_receivers", "antler_manifest_source", "antler_manifest_at"} {
		if after[key] != before[key] {
			t.Fatalf("settings update changed %s: %v -> %v", key, before[key], after[key])
		}
	}

	// An invalid enforcement is rejected rather than stored.
	if _, err := svc.UpdateAntlerSettings(ctx, u.AccountID, d.ID, app.AntlerSettings{Enforcement: "bogus"}); err == nil {
		t.Fatal("a bogus enforcement must be rejected")
	}
	// An empty field leaves the stored value unchanged.
	partial, err := svc.UpdateAntlerSettings(ctx, u.AccountID, d.ID, app.AntlerSettings{Enforcement: "moderate"})
	if err != nil {
		t.Fatalf("partial update: %v", err)
	}
	partialValues, _ := svc.DecryptDomainReceivingConfig(partial)
	if partialValues["contact_email"] != "new@example.test" {
		t.Fatalf("an empty field overwrote the contact: %v", partialValues["contact_email"])
	}
}

// TestDialMXAntlerRejectsSuppliedReceiverURLs proves the Tusk MX receiver set
// is never caller-controlled: an explicit receiver_urls is a validation error
// rather than a silent discard, and no config is written.
func TestDialMXAntlerRejectsSuppliedReceiverURLs(t *testing.T) {
	t.Parallel()
	svc, u, d := newMXServiceWithOutboundPolicy(t, true)
	stub := &stubAntler{receivers: antlerReceivers()}
	svc.AntlerEndpoints = stub
	ctx := context.Background()

	_, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
		"receiver_urls": "https://evil.example.test",
	}, false)
	if !errors.Is(err, app.ErrInvalidConfig) {
		t.Fatalf("err=%v, want ErrInvalidConfig", err)
	}
	if stub.calls != 0 {
		t.Fatalf("rejected save consulted the Tusk manifest %d times", stub.calls)
	}
	if _, err := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, d.ID); err == nil {
		t.Fatal("rejected Tusk save persisted a config")
	}
}

func TestDialMXAntlerRequiresValidContactEmail(t *testing.T) {
	t.Parallel()
	svc, u, d := newMXServiceWithOutboundPolicy(t, true)
	svc.AntlerEndpoints = &stubAntler{receivers: antlerReceivers()}
	ctx := context.Background()
	for _, bad := range []string{"", "not-an-address", "a@b", "ops@example.test extra", "ops@@example.test"} {
		_, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "dialmx", map[string]any{
			"service": mxdial.ServiceAntler, "contact_email": bad,
		}, false)
		if !errors.Is(err, app.ErrInvalidConfig) {
			t.Fatalf("contact %q err=%v, want ErrInvalidConfig", bad, err)
		}
	}
}

func TestDialMXAntlerManifestFailureIsInvalidConfig(t *testing.T) {
	t.Parallel()
	svc, u, d := newMXServiceWithOutboundPolicy(t, true)
	svc.AntlerEndpoints = &stubAntler{err: errors.New("network down")}
	_, _, err := svc.SaveDomainReceivingConfig(context.Background(), u.AccountID, d.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false)
	if !errors.Is(err, app.ErrInvalidConfig) {
		t.Fatalf("err=%v, want ErrInvalidConfig", err)
	}
}

// TestDialMXLegacyCustomSaveStaysCustom proves the service default never
// silently migrates a legacy receiver_urls-only save to Tusk MX.
func TestDialMXLegacyCustomSaveStaysCustom(t *testing.T) {
	t.Parallel()
	svc, u, d := newMXServiceWithOutboundPolicy(t, true)
	stub := &stubAntler{receivers: antlerReceivers()}
	svc.AntlerEndpoints = stub
	ctx := context.Background()

	saved, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "dialmx", map[string]any{
		"receiver_urls": "https://receiver.example",
	}, false)
	if err != nil {
		t.Fatalf("legacy save: %v", err)
	}
	values, err := svc.DecryptDomainReceivingConfig(saved)
	if err != nil {
		t.Fatal(err)
	}
	if values["service"] != mxdial.ServiceCustom {
		t.Fatalf("legacy save service = %v, want custom", values["service"])
	}
	if stub.calls != 0 {
		t.Fatalf("legacy save consulted the Tusk manifest %d times", stub.calls)
	}
	if _, ok := values["contact_email"]; ok {
		t.Fatalf("custom save stored a contact email: %v", values["contact_email"])
	}
	if app.AntlerReceiversFromConfig(values) != nil {
		t.Fatal("custom save has a Tusk snapshot")
	}
}

// TestDialMXSwitchFromCustomToAntler proves an explicit service switch replaces
// the custom URLs with the resolved Tusk snapshot.
func TestDialMXSwitchFromCustomToAntler(t *testing.T) {
	t.Parallel()
	svc, u, d := newMXServiceWithOutboundPolicy(t, true)
	svc.AntlerEndpoints = &stubAntler{receivers: antlerReceivers()}
	ctx := context.Background()
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "dialmx", map[string]any{"receiver_urls": "https://receiver.example"}, false); err != nil {
		t.Fatal(err)
	}
	saved, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	values, err := svc.DecryptDomainReceivingConfig(saved)
	if err != nil {
		t.Fatal(err)
	}
	if values["service"] != mxdial.ServiceAntler || values["receiver_urls"] != "https://antler1.example.test,https://tusk2.example.test" {
		t.Fatalf("switch did not adopt Tusk endpoints: %v", values["receiver_urls"])
	}
}
