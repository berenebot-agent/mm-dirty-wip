package httpapp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/httpapp"
	"github.com/dellarb/mammothmail/internal/mxwire"
	"github.com/dellarb/mammothmail/internal/transport/mxdial"
)

// fixedAntler is a deterministic Tusk endpoint resolver for HTTP tests. A
// resolver with no source answers as the live GitHub manifest did, with a fixed
// fetch time so the rendered manifest line is deterministic.
type fixedAntler struct {
	receivers []mxdial.AntlerReceiver
	source    mxdial.AntlerSource
	fetchedAt time.Time
}

func (f fixedAntler) Receivers(context.Context) (mxdial.AntlerEndpointSet, error) {
	source, at := f.source, f.fetchedAt
	if source == "" {
		source = mxdial.AntlerSourceGitHub
		if at.IsZero() {
			at = time.Date(2026, 10, 18, 5, 12, 3, 0, time.UTC)
		}
	}
	return mxdial.AntlerEndpointSet{Receivers: f.receivers, Source: source, FetchedAt: at}, nil
}

// fakeResolver serves deterministic MX/TXT answers to the watcher.
type fakeResolver struct {
	mx   []*net.MX
	mxE  error
	txt  []string
	txtE error
}

func (f fakeResolver) LookupMX(context.Context, string) ([]*net.MX, error) { return f.mx, f.mxE }
func (f fakeResolver) LookupTXT(context.Context, string) ([]string, error) {
	return f.txt, f.txtE
}

// switchableResolver returns the current answers and counts lookups, so a test
// can prove the watcher re-resolves on each pass while the HTTP layer does not.
type switchableResolver struct {
	mu     sync.Mutex
	mx     []*net.MX
	txt    []string
	mxHits int
	txHits int
}

func (s *switchableResolver) set(mx []*net.MX, txt []string) {
	s.mu.Lock()
	s.mx, s.txt = mx, txt
	s.mu.Unlock()
}

func (s *switchableResolver) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mxHits, s.txHits
}

func (s *switchableResolver) LookupMX(context.Context, string) ([]*net.MX, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mxHits++
	return s.mx, nil
}

func (s *switchableResolver) LookupTXT(context.Context, string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.txHits++
	return s.txt, nil
}

// watchDNS installs the shared DNS watcher and runs one pass, so the receiving
// API and dashboard render read a settled verdict.
func watchDNS(svc *app.Service, res app.DNSResolver) {
	svc.DNS = app.NewDNSWatcher(svc, res)
	svc.DNS.CheckOnce(context.Background())
}

// waitPastDemandFloor waits out the watcher's 10s demand floor so a following
// read has to queue a re-check. shortcut: a 10s sleep per run, fold the floor
// into a test hook if this becomes hot.
func waitPastDemandFloor() { time.Sleep(10 * time.Second) }

// dnsLights returns the kind→state map from a receiving response body.
func dnsLights(t *testing.T, body map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, raw := range body["dns"].([]any) {
		v := raw.(map[string]any)
		out[v["kind"].(string)] = v["state"].(string)
	}
	return out
}

// TestDialMXAntlerReceivingAPITrafficLights proves the receiving API returns the
// Tusk instructions, live receiver status and the watcher's published-record
// checks, with a green verdict for matching MX and TXT records and a red one for a
// mismatch.
func TestDialMXAntlerReceivingAPITrafficLights(t *testing.T) {
	t.Parallel()
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
		{ID: "antler-2", SessionURL: "https://tusk2.example.test", SMTPHostname: "tusk2.example.test", MXPriority: 20},
	}}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	cred, err := svc.Store.GetDialMXCredential(ctx, u.AccountID, domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := base64.RawURLEncoding.DecodeString(cred.PublicKey)
	txtValue := mxwire.DomainTXT(cred.KeyID, pub)

	watchDNS(svc, fakeResolver{
		mx:  []*net.MX{{Host: "antler1.example.test.", Pref: 10}, {Host: "tusk2.example.test.", Pref: 20}},
		txt: []string{txtValue},
	})
	srv := httpapp.New(svc, nil)
	h := srv.Handler()

	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/admin/domains/" + domain.ID + "/receiving"
	do := func() map[string]any {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}
	body := do()
	statuses, _ := body["status"].([]any)
	if len(statuses) != 2 {
		t.Fatalf("must show both receivers before their first connection: %v", body["status"])
	}
	for _, raw := range statuses {
		status := raw.(map[string]any)
		if status["state"] != "connecting" || status["smtp_hostname"] == nil {
			t.Fatalf("unexpected initial receiver status: %v", status)
		}
	}

	instr, _ := body["instructions"].(map[string]any)
	if instr == nil || instr["service"] != "hosted" || instr["contact_email"] != "ops@example.test" {
		t.Fatalf("instructions missing Tusk metadata: %v", body["instructions"])
	}
	mx, _ := instr["mx"].([]any)
	if len(mx) != 2 {
		t.Fatalf("instructions missing MX records: %v", instr["mx"])
	}
	if mx[0].(map[string]any)["hostname"] != "antler1.example.test" {
		t.Fatalf("unexpected MX instruction: %v", mx[0])
	}
	if instr["txt_name"] != "_mail-proof."+domain.Name {
		t.Fatalf("unexpected TXT name: %v", instr["txt_name"])
	}
	// The response says where the receiver set came from, so the wizard can state
	// whether setup used the live GitHub manifest or the built-in defaults.
	manifest, _ := body["receiver_manifest"].(map[string]any)
	if manifest == nil || manifest["source"] != string(mxdial.AntlerSourceGitHub) {
		t.Fatalf("receiver_manifest = %v, want the github source", body["receiver_manifest"])
	}
	if manifest["at"] != "2026-10-18T05:12:03Z" {
		t.Fatalf("receiver_manifest fetch time = %v", manifest["at"])
	}

	lights := dnsLights(t, body)
	if lights["mx"] != "ok" || lights["txt"] != "ok" {
		t.Fatalf("expected green MX and TXT lights, got %v", lights)
	}

	// A wrong key turns the TXT light red; a foreign MX hostname turns MX red.
	if _, err := svc.RotateDialMXCredential(ctx, u.AccountID, domain.ID); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	watchDNS(svc, fakeResolver{
		mx:  []*net.MX{{Host: "elsewhere.example.test.", Pref: 10}},
		txt: []string{"v=MM1; k=ed25519; id=other; p=" + base64.StdEncoding.EncodeToString(pub)},
	})
	for kind, state := range dnsLights(t, do()) {
		if state != "mismatch" {
			t.Fatalf("%s light = %v, want mismatch", kind, state)
		}
	}

	// The dashboard setup panel renders the MX instructions and the lights.
	cookie, _ := uiSession(t, svc, u.ID)
	page := domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving&provider=dialmx")
	for _, want := range []string{"Tusk MX", "10 antler1.example.test", "ops@example.test", "dns-light"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Fatalf("setup panel missing %q", want)
		}
	}
	// The status view draws its connector table from the set embedded on the
	// form, so the rendered attributes must exist and name a connector hostname
	// (html/template escapes them for attribute context).
	for _, attr := range []string{`data-antler-connectors=`, `data-antler-status=`} {
		i := strings.Index(page.Body.String(), attr)
		if i < 0 {
			t.Fatalf("receiving form missing %s attribute", attr)
		}
		attrVal := page.Body.String()[i:]
		if end := strings.IndexByte(attrVal, '>'); end >= 0 {
			attrVal = attrVal[:end]
		}
		if !strings.Contains(attrVal, "antler1.example.test") {
			t.Fatalf("%s data missing receiver hostname: %q", attr, attrVal)
		}
	}
}

// TestDialMXWatcherRefreshesVerdict proves the shared verdict is the watcher's:
// each pass re-resolves DNS, so a record changed between two passes is reflected
// in the receiving API rather than served from a stale cache.
func TestDialMXWatcherRefreshesVerdict(t *testing.T) {
	t.Parallel()
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
	}}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	cred, err := svc.Store.GetDialMXCredential(ctx, u.AccountID, domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := base64.RawURLEncoding.DecodeString(cred.PublicKey)
	txtValue := mxwire.DomainTXT(cred.KeyID, pub)

	res := &switchableResolver{}
	res.set([]*net.MX{{Host: "antler1.example.test.", Pref: 10}}, []string{txtValue})
	watchDNS(svc, res)

	srv := httpapp.New(svc, nil)
	h := srv.Handler()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/admin/domains/" + domain.ID + "/receiving"
	do := func() map[string]any {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}

	if got := dnsLights(t, do())["txt"]; got != "ok" {
		t.Fatalf("first TXT light = %q, want ok", got)
	}
	// The record changes to a key that no longer matches the domain's exact key;
	// the next watcher pass republishes the verdict.
	res.set([]*net.MX{{Host: "antler1.example.test.", Pref: 10}}, []string{
		"v=MM1; k=ed25519; id=other; p=" + base64.StdEncoding.EncodeToString(pub),
	})
	svc.DNS.CheckOnce(ctx)
	if got := dnsLights(t, do())["txt"]; got != "mismatch" {
		t.Fatalf("second TXT light = %q, want mismatch (a stale verdict would stay ok)", got)
	}
}

// TestDialMXReceivingStatusRunsNoLookup proves the HTTP layer reads the watcher's
// cached verdict and runs no resolver lookup of its own: repeated status reads do
// not move the resolver counters.
func TestDialMXReceivingStatusRunsNoLookup(t *testing.T) {
	t.Parallel()
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
	}}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	cred, err := svc.Store.GetDialMXCredential(ctx, u.AccountID, domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := base64.RawURLEncoding.DecodeString(cred.PublicKey)
	res := &switchableResolver{}
	res.set([]*net.MX{{Host: "antler1.example.test.", Pref: 10}}, []string{mxwire.DomainTXT(cred.KeyID, pub)})
	watchDNS(svc, res)
	baseMX, baseTX := res.counts()

	srv := httpapp.New(svc, nil)
	h := srv.Handler()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/domains/"+domain.ID+"/receiving", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, ok := body["dns"]; !ok {
			t.Fatalf("status read must carry the watcher's cached verdict, got %v", body["dns"])
		}
	}
	if mxHits, txHits := res.counts(); mxHits != baseMX || txHits != baseTX {
		t.Fatalf("status reads ran resolver lookups: mx %d->%d txt %d->%d", baseMX, mxHits, baseTX, txHits)
	}
}

// TestDialMXExtraMXVerdictIsSharedAcrossSurfaces proves the status dialog's
// warning field and the dashboard light's verdict come from the one shared
// watcher-owned value, so the two surfaces cannot disagree about whether a rogue
// MX record exists.
func TestDialMXExtraMXVerdictIsSharedAcrossSurfaces(t *testing.T) {
	t.Parallel()
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
	}}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	// A ready receiver and a rogue MX record appear together: amber, with the
	// rogue host named.
	m := mxdial.New(svc.DialMXBackend(), mxdial.Config{DataDir: svc.Config.DataDir, AllowPrivateDestinations: true})
	m.SetStatusForTest(domain.Name, []mxdial.Status{{
		ReceiverURL: "https://antler1.example.test", State: mxdial.StatusActive, SMTPHostname: "antler1.example.test",
	}})
	svc.DialMX = m
	watchDNS(svc, fakeResolver{mx: []*net.MX{
		{Host: "antler1.example.test.", Pref: 10},
		{Host: "old.example.test.", Pref: 20},
	}})

	srv := httpapp.New(svc, nil)
	h := srv.Handler()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The status dialog's read carries the shared verdict at the top level.
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/domains/"+domain.ID+"/receiving", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, _ := body["unexpected_mx"].([]any)
	if len(got) != 1 || got[0] != "old.example.test" {
		t.Fatalf("top-level unexpected_mx = %v, want [old.example.test]", body["unexpected_mx"])
	}

	// The dashboard light's read must agree with the same shared verdict.
	cookie, _ := uiSession(t, svc, u.ID)
	req = httptest.NewRequest(http.MethodGet, "/ui/state?page=dashboard&lights=1", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var snap struct {
		Domains map[string]struct {
			Light string `json:"light"`
		} `json:"domains"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode snapshot: %v body=%s", err, rr.Body.String())
	}
	if l := snap.Domains[domain.ID].Light; l != "amber" {
		t.Fatalf("dashboard light = %q, want amber (same verdict as the dialog)", l)
	}
}

// TestDialMXStaleVerdictSelfClears proves a removed rogue MX record clears the
// "additional MX servers" warning without waiting for the next poll: the read
// that serves a stale verdict queues a re-check and says so (dns_stale), so the
// status dialog's own follow-up read carries the fresh, empty verdict.
func TestDialMXStaleVerdictSelfClears(t *testing.T) {
	// Serial: it ages the watcher's verdict past the demand floor.
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
	}}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	res := &switchableResolver{}
	res.set([]*net.MX{{Host: "antler1.example.test.", Pref: 10}, {Host: "old.example.test.", Pref: 20}}, nil)
	watchDNS(svc, res)
	srv := httpapp.New(svc, nil)
	h := srv.Handler()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	get := func() map[string]any {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/domains/"+domain.ID+"/receiving", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v body=%s", err, rr.Body.String())
		}
		return body
	}
	if got, _ := get()["unexpected_mx"].([]any); len(got) != 1 {
		t.Fatalf("extra-MX before the fix = %v, want [old.example.test]", got)
	}

	// The operator removes the record; the cached verdict is now stale.
	res.set([]*net.MX{{Host: "antler1.example.test.", Pref: 10}}, nil)
	waitPastDemandFloor()
	if !svc.DNS.Stale(domain.ID) {
		t.Fatal("verdict must read as stale once it is past the demand floor")
	}
	first := get()
	if first["dns_stale"] != true {
		t.Fatalf("dns_stale = %v, want true so the dialog re-reads", first["dns_stale"])
	}
	svc.DNS.CheckDemandedOnce(ctx)
	second := get()
	if second["dns_stale"] == true {
		t.Fatalf("dns_stale persisted after the re-check: %v", second["dns_stale"])
	}
	if got, _ := second["unexpected_mx"].([]any); len(got) != 0 {
		t.Fatalf("extra-MX after the follow-up read = %v, want none", got)
	}
}

// formAttr returns the value of one quoted attribute on the receiving form.// html/template escapes any inner quote as &#34;, so the next quote ends this
// attribute's value — cutting at the tag's ">" instead would run into every
// later attribute (including data-antler-dns, whose extra-MX verdict would then
// satisfy an assertion meant for data-antler-unexpected alone).
func formAttr(t *testing.T, body, attr string) string {
	t.Helper()
	i := strings.Index(body, attr)
	if i < 0 {
		t.Fatalf("receiving form missing %s attribute", attr)
	}
	rest := strings.TrimPrefix(body[i+len(attr):], `"`)
	if end := strings.IndexByte(rest, '"'); end >= 0 {
		return rest[:end]
	}
	return rest
}

// TestDialMXStatusViewSeedsFixPrompts proves the receiving form embeds the
// copy-ready instructions, the extra-MX verdict and the DNS checks so the saved
// status view can paint its fix prompts and warning on open — rather than showing
// rows without their remediation until the first fetch lands. It is what makes a
// red receiver's DNS problem appear immediately, matching the dashboard light.
func TestDialMXStatusViewSeedsFixPrompts(t *testing.T) {
	t.Parallel()
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
	}}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	cred, err := svc.Store.GetDialMXCredential(ctx, u.AccountID, domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := base64.RawURLEncoding.DecodeString(cred.PublicKey)
	// A ready receiver, a rogue MX record and a broken TXT: the form must seed the
	// instructions, the rogue-host warning and both DNS checks.
	m := mxdial.New(svc.DialMXBackend(), mxdial.Config{DataDir: svc.Config.DataDir, AllowPrivateDestinations: true})
	m.SetStatusForTest(domain.Name, []mxdial.Status{{
		ReceiverURL: "https://antler1.example.test", State: mxdial.StatusActive, SMTPHostname: "antler1.example.test",
	}})
	svc.DialMX = m
	watchDNS(svc, fakeResolver{
		mx:  []*net.MX{{Host: "antler1.example.test.", Pref: 10}, {Host: "old.example.test.", Pref: 20}},
		txt: []string{"v=MM1; k=ed25519; id=other; p=" + base64.StdEncoding.EncodeToString(pub)},
	})

	srv := httpapp.New(svc, nil)
	h := srv.Handler()
	cookie, _ := uiSession(t, svc, u.ID)
	page := domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving&provider=dialmx")

	body := page.Body.String()
	attrValue := func(attr string) string { return formAttr(t, body, attr) }
	// The copy-ready instructions carry the MX record the operator must publish.
	if got := attrValue(`data-antler-instructions=`); !strings.Contains(got, "antler1.example.test") {
		t.Fatalf("seeded instructions missing the MX record: %q", got)
	}
	// The shared extra-MX verdict names the rogue host.
	if got := attrValue(`data-antler-unexpected=`); !strings.Contains(got, "old.example.test") {
		t.Fatalf("seeded extra-MX verdict missing the rogue host: %q", got)
	}
	// The DNS checks flag both the TXT mismatch and the rogue MX.
	if got := attrValue(`data-antler-dns=`); !strings.Contains(got, "mismatch") || !strings.Contains(got, "old.example.test") {
		t.Fatalf("seeded DNS checks missing the TXT mismatch or rogue host: %q", got)
	}
	// The manifest record seeds too, so the status view says where its receiver set
	// came from the moment it opens. html/template escapes the inner quotes for the
	// attribute context, so this reads the values rather than the JSON spelling.
	if got := attrValue(`data-antler-manifest=`); !strings.Contains(got, "github") || !strings.Contains(got, "2026-10-18T05:12:03Z") {
		t.Fatalf("seeded manifest record missing its source or fetch time: %q", got)
	}
}

// TestDialMXStatusViewSeedsExtraMXWithoutAReadyReceiver proves the status view
// seeds the shared extra-MX warning even when nothing has authenticated yet. The
// warning is a fix prompt ("remove these records"), so it must not wait for the
// dialog's own fetch just because the domain is still red — and the dashboard
// light must stay red, since the extra-MX verdict only turns a ready domain amber.
func TestDialMXStatusViewSeedsExtraMXWithoutAReadyReceiver(t *testing.T) {
	t.Parallel()
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
	}}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	// No receiver status is injected and no dial manager is installed, so the
	// domain is configured but nothing has authenticated: the light is red.
	watchDNS(svc, fakeResolver{mx: []*net.MX{
		{Host: "antler1.example.test.", Pref: 10},
		{Host: "old.example.test.", Pref: 20},
	}})

	srv := httpapp.New(svc, nil)
	h := srv.Handler()
	cookie, _ := uiSession(t, svc, u.ID)
	page := domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving&provider=dialmx")
	body := page.Body.String()

	// The light is red, not amber: an extra MX record never turns an unready
	// domain green or amber.
	if !strings.Contains(body, `data-kind="receiving" data-receiving-button><span class="dns-light danger"`) {
		t.Fatalf("receiving light should be danger with no ready receiver")
	}
	// The rogue host is seeded on the form, so the warning paints on open.
	if got := formAttr(t, body, `data-antler-unexpected=`); !strings.Contains(got, "old.example.test") {
		t.Fatalf("seeded extra-MX verdict missing the rogue host: %q", got)
	}
}

// TestDialMXAntlerPartialMXSetIsHealthy proves the MX check treats the receiver
// set as redundancy: publishing only one of two advertised receivers is "ok",
// matched lists the receiver that is actually pointed at, and the other is left
// out of matched (so its connector row is not painted by the aggregate check).
func TestDialMXAntlerPartialMXSetIsHealthy(t *testing.T) {
	t.Parallel()
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
		{ID: "antler-2", SessionURL: "https://tusk2.example.test", SMTPHostname: "tusk2.example.test", MXPriority: 20},
	}}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	cred, err := svc.Store.GetDialMXCredential(ctx, u.AccountID, domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := base64.RawURLEncoding.DecodeString(cred.PublicKey)
	// Only the priority-10 receiver is published.
	watchDNS(svc, fakeResolver{
		mx:  []*net.MX{{Host: "antler1.example.test.", Pref: 10}},
		txt: []string{mxwire.DomainTXT(cred.KeyID, pub)},
	})
	srv := httpapp.New(svc, nil)
	h := srv.Handler()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/domains/"+domain.ID+"/receiving", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var mx *map[string]any
	for _, raw := range body["dns"].([]any) {
		v := raw.(map[string]any)
		if v["kind"] == "mx" {
			m := v
			mx = &m
		}
	}
	if mx == nil {
		t.Fatalf("no MX check returned: %v", body["dns"])
	}
	if (*mx)["state"] != "ok" {
		t.Fatalf("partial MX set = %v, want ok", (*mx)["state"])
	}
	matched, _ := (*mx)["matched"].([]any)
	if len(matched) != 1 || matched[0] != "antler1.example.test" {
		t.Fatalf("matched = %v, want only the published receiver", matched)
	}
}

// The browser uses session-scoped setup routes; saves require the existing
// CSRF guard, and reads require a browser session.
func TestTuskWizardSessionSetup(t *testing.T) {
	t.Parallel()
	svc, h, u, domain, _ := httpFixture(t)
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
	}}
	cookie, csrf := uiSession(t, svc, u.ID)
	path := "/ui/domains/" + domain.ID + "/receiving/setup"
	do := func(method, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(`{"provider":"dialmx","config":{"service":"hosted","contact_email":"ops@example.test","enforcement":"moderate"}}`))
		req.AddCookie(cookie)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", token)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := do(http.MethodPut, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("save without CSRF: %d %s", rr.Code, rr.Body.String())
	}
	if rr := do(http.MethodPut, csrf); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"txt_value"`) {
		t.Fatalf("session setup save: %d %s", rr.Code, rr.Body.String())
	}
	if rr := do(http.MethodGet, ""); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"smtp_hostname":"antler1.example.test"`) {
		t.Fatalf("session status check: %d %s", rr.Code, rr.Body.String())
	}
	before, err := svc.Store.GetDomainReceivingConfig(context.Background(), u.AccountID, domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeValues, err := svc.DecryptDomainReceivingConfig(before)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := svc.Store.GetDialMXCredential(context.Background(), u.AccountID, domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A changed manifest must not change endpoints during an email-only save.
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "new", SessionURL: "https://new.example.test", SMTPHostname: "new.example.test", MXPriority: 10},
	}}
	update := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"provider":"dialmx","config":{"contact_email":"changed@example.test","enforcement":"hard"}}`))
		req.AddCookie(cookie)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", token)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := update(""); rr.Code != http.StatusForbidden {
		t.Fatalf("email save without CSRF: %d", rr.Code)
	}
	if rr := update(csrf); rr.Code != http.StatusOK {
		t.Fatalf("email-only save: %d %s", rr.Code, rr.Body.String())
	}
	after, err := svc.Store.GetDomainReceivingConfig(context.Background(), u.AccountID, domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterValues, err := svc.DecryptDomainReceivingConfig(after)
	if err != nil {
		t.Fatal(err)
	}
	if afterValues["contact_email"] != "changed@example.test" {
		t.Fatalf("email not saved: %v", afterValues)
	}
	if afterValues["enforcement"] != "hard" {
		t.Fatalf("enforcement not saved in the same request: %v", afterValues["enforcement"])
	}
	for _, field := range []string{"receiver_urls", "antler_receivers", "setup_id", "antler_manifest_source", "antler_manifest_at"} {
		if afterValues[field] != beforeValues[field] {
			t.Fatalf("settings save changed %s", field)
		}
	}
	afterCredential, err := svc.Store.GetDialMXCredential(context.Background(), u.AccountID, domain.ID)
	if err != nil || afterCredential.KeyID != credential.KeyID {
		t.Fatalf("email save changed domain key: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("setup must require a session: %d", rr.Code)
	}
}

// TestDialMXAntlerDialogHidesReceiverURLs pins that the editable Receiver URLs
// field is not present in the Tusk MX dialog (it lives only in an inert
// template), but is present and editable for the custom service.
func TestDialMXAntlerDialogHidesReceiverURLs(t *testing.T) {
	t.Parallel()
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
	}}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	srv := httpapp.New(svc, nil)
	h := srv.Handler()
	cookie, _ := uiSession(t, svc, u.ID)
	body := dialogHTML(t, domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving&provider=dialmx").Body.String(), "domain-receiving-dialog-"+domain.ID)
	settings := providerGroupHTML(t, body, "dialmx")

	if strings.Contains(stripDialMXReceiverTemplate(settings), `name="cfg_dialmx_receiver_urls"`) {
		t.Fatalf("Tusk dialog renders an editable receiver_urls input:\n%s", settings)
	}
	if !strings.Contains(settings, `<template class="dialmx-receiver-urls-field">`) {
		t.Fatalf("Tusk dialog is missing the inert receiver_urls template:\n%s", settings)
	}

	// A custom setup keeps the editable input.
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceCustom, "receiver_urls": "https://receiver.example",
	}, false); err != nil {
		t.Fatalf("custom save: %v", err)
	}
	body = dialogHTML(t, domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving&provider=dialmx").Body.String(), "domain-receiving-dialog-"+domain.ID)
	settings = providerGroupHTML(t, body, "dialmx")
	if !strings.Contains(settings, `name="cfg_dialmx_receiver_urls"`) {
		t.Fatalf("custom dialog must render the editable receiver_urls input:\n%s", settings)
	}
	if strings.Contains(settings, `<template class="dialmx-receiver-urls-field">`) {
		t.Fatalf("custom dialog must not wrap the field in a template:\n%s", settings)
	}
}

// TestDialMXHostedAPIRejectsReceiverURLs proves the receiving API refuses a
// client-supplied receiver_urls for the Tusk service, so a crafted request
// cannot point the domain at an arbitrary receiver.
func TestDialMXHostedAPIRejectsReceiverURLs(t *testing.T) {
	t.Parallel()
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
	}}
	srv := httpapp.New(svc, nil)
	h := srv.Handler()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/admin/domains/" + domain.ID + "/receiving"
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{"provider":"dialmx","config":{"service":"hosted","contact_email":"ops@example.test","receiver_urls":"https://evil.example.test"}}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "Tusk MX") {
		t.Fatalf("injected receiver_urls for hosted Tusk MX: %d %s", rr.Code, rr.Body.String())
	}
	// The rejected save must not have replaced the fixture's existing provider
	// nor stored the injected URL.
	cfg, err := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, domain.ID)
	if err != nil || cfg.Provider != "mailgun" {
		t.Fatalf("rejected save changed the provider: %+v %v", cfg, err)
	}
}

// TestDialMXReceivingAPIRejectsReceiverURLsArray proves the receiving API takes
// receiver_urls as the comma-separated string the API reference documents, and
// refuses the array form a caller guessing from the field name would send.
func TestDialMXReceivingAPIRejectsReceiverURLsArray(t *testing.T) {
	t.Parallel()
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	srv := httpapp.New(svc, nil)
	h := srv.Handler()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/admin/domains/" + domain.ID + "/receiving"
	put := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	// The array form is refused with the type error.
	rr := put(`{"provider":"dialmx","config":{"service":"custom","receiver_urls":["https://receiver.example"]}}`)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "must be a string") {
		t.Fatalf("array receiver_urls: %d %s", rr.Code, rr.Body.String())
	}
	// The string form is accepted and stored as a comma-separated string.
	rr = put(`{"provider":"dialmx","config":{"service":"custom","receiver_urls":"https://receiver.example"}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("string receiver_urls: %d %s", rr.Code, rr.Body.String())
	}
	cfg, err := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	values, err := svc.DecryptDomainReceivingConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if values["receiver_urls"] != "https://receiver.example" {
		t.Fatalf("stored receiver_urls = %v", values["receiver_urls"])
	}
}

// TestDialMXCustomReceivingAPIHasNoAntlerInstructions pins that a custom setup
// keeps its manual receiver URLs and does not advertise Tusk MX records, and
// that the watcher computes no MX check for it (the MX hostname is the operator's
// own), only the TXT check.
func TestDialMXCustomReceivingAPIHasNoAntlerInstructions(t *testing.T) {
	t.Parallel()
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{"receiver_urls": "https://receiver.example"}, false); err != nil {
		t.Fatal(err)
	}
	watchDNS(svc, fakeResolver{mx: []*net.MX{{Host: "receiver.example.", Pref: 10}}})
	srv := httpapp.New(svc, nil)
	h := srv.Handler()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/domains/"+domain.ID+"/receiving", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	instr, _ := body["instructions"].(map[string]any)
	if instr == nil || instr["service"] != "custom" {
		t.Fatalf("instructions = %v, want custom", body["instructions"])
	}
	if _, ok := instr["mx"]; ok {
		t.Fatalf("custom setup advertised MX instructions: %v", instr["mx"])
	}
	for _, raw := range body["dns"].([]any) {
		v := raw.(map[string]any)
		if v["kind"] == "mx" {
			t.Fatalf("custom setup should not check MX records: %v", v)
		}
	}

	// The dialog must prefill the custom service so a save cannot silently
	// migrate a legacy configuration to Tusk MX.
	cookie, _ := uiSession(t, svc, u.ID)
	page := domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving&provider=dialmx")
	settings := providerGroupHTML(t, dialogHTML(t, page.Body.String(), "domain-receiving-dialog-"+domain.ID), "dialmx")
	if !strings.Contains(settings, `<option value="custom" selected>`) {
		t.Fatalf("legacy custom config not prefilled as custom:\n%s", settings)
	}
}

// stripDialMXReceiverTemplate removes the inert <template> that carries the
// Receiver URLs field for a Tusk MX setup, so an assertion can check that no
// editable input is rendered without tripping on the template's own input.
func stripDialMXReceiverTemplate(settings string) string {
	const open = `<template class="dialmx-receiver-urls-field">`
	start := strings.Index(settings, open)
	if start < 0 {
		return settings
	}
	rest := settings[start:]
	if end := strings.Index(rest, "</template>"); end >= 0 {
		return settings[:start] + rest[end+len("</template>"):]
	}
	return settings[:start]
}
