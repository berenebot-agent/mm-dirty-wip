package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// stubAntler is a deterministic endpoint resolver for save-flow tests.
type stubAntler struct {
	receivers []mxdial.AntlerReceiver
	err       error
	calls     int
}

func (s *stubAntler) Receivers(context.Context) ([]mxdial.AntlerReceiver, error) {
	s.calls++
	return s.receivers, s.err
}

func antlerReceivers() []mxdial.AntlerReceiver {
	return []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
		{ID: "antler-2", SessionURL: "https://antler2.example.test", SMTPHostname: "antler2.example.test", MXPriority: 20},
	}
}

// TestDialMXAntlerSetupSnapshotsEndpoints proves an Antler MX setup validates
// the contact email, resolves the live manifest, snapshots both receiver URLs
// and hostnames, and assigns a stable setup id that a resave retains.
func TestDialMXAntlerSetupSnapshotsEndpoints(t *testing.T) {
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
	if values["receiver_urls"] != "https://antler1.example.test,https://antler2.example.test" {
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
}

func TestDialMXAntlerRequiresValidContactEmail(t *testing.T) {
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
// silently migrates a legacy receiver_urls-only save to Antler MX.
func TestDialMXLegacyCustomSaveStaysCustom(t *testing.T) {
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
		t.Fatalf("legacy save consulted the Antler manifest %d times", stub.calls)
	}
	if _, ok := values["contact_email"]; ok {
		t.Fatalf("custom save stored a contact email: %v", values["contact_email"])
	}
	if app.AntlerReceiversFromConfig(values) != nil {
		t.Fatal("custom save has an Antler snapshot")
	}
}

// TestDialMXSwitchFromCustomToAntler proves an explicit service switch replaces
// the custom URLs with the resolved Antler snapshot.
func TestDialMXSwitchFromCustomToAntler(t *testing.T) {
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
	if values["service"] != mxdial.ServiceAntler || values["receiver_urls"] != "https://antler1.example.test,https://antler2.example.test" {
		t.Fatalf("switch did not adopt Antler endpoints: %v", values["receiver_urls"])
	}
}
