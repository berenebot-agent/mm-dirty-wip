package httpapp_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gatehouse-mail/internal/httpapp"
)

func inboundRawMessage() string {
	return strings.Join([]string{
		"From: sender@outside.test",
		"To: hermes@example.com",
		"Subject: inbound listener",
		"Message-ID: <inbound-listener@test>",
		"MIME-Version: 1.0",
		"Content-Type: text/plain",
		"",
		"hello",
		"",
	}, "\r\n")
}

func TestInboundHandlerExposesOnlyConnector(t *testing.T) {
	svc, _, _, _, _ := httpFixture(t)
	h := httpapp.New(svc, nil).InboundHandler()

	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{"GET", "/healthz", http.StatusOK},
		{"GET", "/", http.StatusNotFound},
		{"GET", "/dashboard", http.StatusNotFound},
		{"GET", "/v1/bootstrap", http.StatusNotFound},
		{"GET", "/relay", http.StatusNotFound},
		{"GET", "/assets/app.js", http.StatusNotFound},
		{"GET", "/assets/logo-horizontal.png", http.StatusNotFound},
		{"GET", "/favicon.ico", http.StatusNotFound},
		{"GET", "/openapi.json", http.StatusNotFound},
		{"GET", "/internal/ingest/mailgun", http.StatusMethodNotAllowed},
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
		if rr.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rr.Code, tc.want)
		}
	}
}

func TestInboundHandlerAcceptsWebhook(t *testing.T) {
	svc, _, _, _, box := httpFixture(t)
	h := httpapp.New(svc, nil).InboundHandler()

	req := signedMGRequest(t, testMailgunKey, "inbound-dedicated", box.Address, inboundRawMessage())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("inbound webhook = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestMainHandlerStillServesInbound(t *testing.T) {
	_, h, _, _, box := httpFixture(t)

	req := signedMGRequest(t, testMailgunKey, "inbound-main", box.Address, inboundRawMessage())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("main handler inbound webhook = %d body=%s", rr.Code, rr.Body.String())
	}
}
