package httpapp_test

import (
	"bytes"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mammothmail/internal/httpapp"
)

func TestBrandAssetsServed(t *testing.T) {
	t.Parallel()
	_, h, _, _, _ := httpFixture(t)
	for _, tc := range []struct {
		path string
		ct   string
	}{
		{"/favicon.ico", "image/x-icon"},
		{"/favicon-16x16.png", "image/png"},
		{"/favicon-32x32.png", "image/png"},
		{"/apple-touch-icon.png", "image/png"},
		{"/assets/logo-horizontal.png", "image/png"},
		{"/assets/tusk-mx-diagram.svg", "image/svg+xml"},
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", tc.path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s = %d", tc.path, rr.Code)
		}
		if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, tc.ct) {
			t.Fatalf("%s content-type = %q, want %q", tc.path, got, tc.ct)
		}
	}
}

// The Tusk MX setup wizard opens with a short service intro and the mail-flow
// diagram, mirroring docs/EMAIL_PROVIDERS.md. The wizard markup is built in
// app.js, so assert the intro and its asset reference survive there.
func TestTuskWizardIntro(t *testing.T) {
	t.Parallel()
	bundle := string(httpapp.AppJS())
	for _, want := range []string{
		"/assets/tusk-mx-diagram.svg",
		"free hosted SMTP receiver for your domain",
		"outbound connection",
		"Tusk MX is a community service, provided on a best-efforts basis",
	} {
		if !strings.Contains(bundle, want) {
			t.Fatalf("Tusk wizard intro missing %q", want)
		}
	}
}

// The tab icons must keep real transparency. An icon exported onto an opaque
// tile (a palette PNG whose tRNS is 0xFF for every entry, or 32 bpp with alpha
// bytes all 255) renders as a white chip on a dark tab strip, which is how this
// bug shipped once already.
func TestFaviconKeepsTransparency(t *testing.T) {
	t.Parallel()
	_, h, _, _, _ := httpFixture(t)
	for _, path := range []string{"/favicon-16x16.png", "/favicon-32x32.png"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		img, err := png.Decode(bytes.NewReader(rr.Body.Bytes()))
		if err != nil {
			t.Fatalf("%s is not a decodable PNG: %v", path, err)
		}
		var clear, solid int
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				_, _, _, a := img.At(x, y).RGBA()
				switch {
				case a == 0:
					clear++
				case a >= 0xf000: // RGBA() is 16-bit; 0xf000 is ~94% opaque
					solid++
				}
			}
		}
		if clear == 0 {
			t.Fatalf("%s has no transparent pixel: the background was flattened into the tile", path)
		}
		if solid == 0 {
			t.Fatalf("%s has no near-opaque pixel: the artwork is missing", path)
		}
	}
}

// iOS paints transparency as black, so the touch icon stays opaque; a
// transparent one is a regression rather than a fix.
func TestAppleTouchIconStaysOpaque(t *testing.T) {
	t.Parallel()
	_, h, _, _, _ := httpFixture(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/apple-touch-icon.png", nil))
	img, err := png.Decode(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("apple-touch-icon.png is not a decodable PNG: %v", err)
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
				t.Fatalf("apple-touch-icon.png has a translucent pixel at %d,%d", x, y)
			}
		}
	}
}

// Browsers cache favicons far past normal asset lifetimes and ignore a plain
// reload, so every icon link carries the build's asset version. Without it a
// corrected icon is invisible after deploy and looks like the fix failed.
func TestFaviconLinksAreVersioned(t *testing.T) {
	t.Parallel()
	_, h, _, _, _ := httpFixture(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/login", nil))
	body := rr.Body.String()
	for _, want := range []string{
		`href="/favicon.ico?v=`,
		`href="/favicon-16x16.png?v=`,
		`href="/favicon-32x32.png?v=`,
		`href="/apple-touch-icon.png?v=`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("head icon link missing version query: %q", want)
		}
	}
}

func TestHeaderUsesLogoHomeLink(t *testing.T) {
	t.Parallel()
	_, h, _, _, _ := httpFixture(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/login", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/login = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "logo-horizontal.png") {
		t.Fatalf("header missing logo image")
	}
	if !strings.Contains(body, `rel="icon"`) {
		t.Fatalf("head missing favicon link")
	}
	if strings.Contains(body, "<b>Mammoth Mail</b>") || strings.Contains(body, "<b>Mammoth Mail</b>") {
		t.Fatalf("header still renders a text brand label")
	}
}
