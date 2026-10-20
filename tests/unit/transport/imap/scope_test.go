package imap_test

import (
	"testing"

	imapadapter "github.com/dellarb/mailmoose/internal/transport/imap"
)

// TestRootScopeExcludesSharedNamespaces proves a personal scope never admits a
// shared/other namespace, even when the personal prefix is empty (INBOX at the
// top level). A shared prefix and everything under it are out of scope, while
// ordinary personal folders (INBOX and its siblings) remain in scope.
func TestRootScopeExcludesSharedNamespaces(t *testing.T) {
	scope := imapadapter.RootScope{
		Root:            "",
		Delimiter:       '/',
		Personal:        true,
		INBOXInScope:    true,
		ExcludePrefixes: []string{"Shared", "Other Users"},
	}
	inScope := []string{"INBOX", "Sent", "Drafts", "Archive", "Custom/Sub"}
	for _, p := range inScope {
		if !scope.InScope(p) {
			t.Fatalf("personal folder %q wrongly out of scope", p)
		}
	}
	outScope := []string{"Shared", "Shared/team/mailbox", "Other Users/bob", "Other Users/bob/INBOX"}
	for _, p := range outScope {
		if scope.InScope(p) {
			t.Fatalf("shared namespace %q wrongly in scope", p)
		}
	}
	if scope.InScope("") {
		t.Fatal("empty root path must not be in scope")
	}
}

// TestRootScopeNonEmptyPrefixBounds proves a non-empty personal prefix bounds the
// namespace to that prefix and its children (a shared prefix still excluded).
func TestRootScopeNonEmptyPrefixBounds(t *testing.T) {
	scope := imapadapter.RootScope{
		Root:            "INBOX.",
		Delimiter:       '.',
		Personal:        true,
		INBOXInScope:    true,
		ExcludePrefixes: []string{"Other Users."},
	}
	for _, p := range []string{"INBOX", "INBOX.Sent", "INBOX.Drafts"} {
		if !scope.InScope(p) {
			t.Fatalf("%q wrongly out of scope", p)
		}
	}
	for _, p := range []string{"Other Users.bob", "Shared.x", "Outside"} {
		if scope.InScope(p) {
			t.Fatalf("%q wrongly in scope", p)
		}
	}
}

// TestRootScopeExcludesTerminatedPrefixes proves a namespace descriptor whose
// prefix already ends with the delimiter (for example "Shared/" or
// "Other Users.") is still excluded, even when it uses a different delimiter
// style to the session and the personal prefix is empty. This is the case a
// naive delimiter-child check misses.
func TestRootScopeExcludesTerminatedPrefixes(t *testing.T) {
	scope := imapadapter.RootScope{
		Root:            "",
		Delimiter:       '/',
		Personal:        true,
		INBOXInScope:    true,
		ExcludePrefixes: []string{"Shared/", "Other Users/", "Other Users."},
	}
	for _, p := range []string{"INBOX", "Sent", "Archive", "Custom/Sub"} {
		if !scope.InScope(p) {
			t.Fatalf("personal folder %q wrongly out of scope", p)
		}
	}
	for _, p := range []string{"Shared", "Shared/team", "Shared/team/INBOX", "Other Users/bob", "Other Users.bob", "Other Users.bob.INBOX"} {
		if scope.InScope(p) {
			t.Fatalf("terminated shared prefix wrongly admitted %q", p)
		}
	}
}

// TestRootScopeConservativeStrictRoot proves that without a discovered personal
// namespace the scope is conservative: only the explicit root and its children,
// never the whole login.
func TestRootScopeConservativeStrictRoot(t *testing.T) {
	scope := imapadapter.RootScope{Root: "INBOX", Delimiter: '/', Personal: false, INBOXInScope: true}
	if !scope.InScope("INBOX") || !scope.InScope("INBOX/2024") {
		t.Fatal("explicit root and its children must be in scope")
	}
	for _, p := range []string{"Sent", "Shared/team", "Other Users/bob"} {
		if scope.InScope(p) {
			t.Fatalf("conservative strict root wrongly admitted %q", p)
		}
	}
}
