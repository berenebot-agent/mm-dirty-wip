#!/bin/bash
# tests/security-live-check.sh — read-only live probes for the security review
# findings 3-9 (see the review paste; findings 1-2 are handled elsewhere).
#
# This script is a MANUAL diagnostic. It is not part of ./tests/run.sh and not
# part of CI. It sends requests to a RUNNING MailMoose instance and prints, for
# each finding, the observation that decides whether the finding reproduces.
#
# It is deliberately non-destructive:
#   * it never creates accounts, sends mail or changes settings;
#   * finding 7's account probe fires at most a handful of *failed* logins;
#   * finding 8's flood is opt-in (--argon-flood) because it is a load test.
#
# Usage:
#   ./tests/security-live-check.sh --base https://mail.example.com \
#       --email you@example.com [--cookie jar.txt] [--compose-inbox <id>]
#       [--insecure] [--argon-flood] [--anon-inbound]
#
#   --base           Base URL of the instance (default: $BASE_URL, else
#                    http://localhost:8081).
#   --email          An account email, used ONLY for the per-account login
#                    throttle probe (finding 7). The password is always wrong,
#                    so this is safe: it must not lock a real user out.
#   --cookie         A Netscape cookie jar from a signed-in browser session, if
#                    you have one. Needed for the authenticated checks (5).
#   --compose-inbox  Inbox id to request the compose page for (finding 5).
#   --insecure       Pass -k to curl (self-signed TLS).
#   --argon-flood    Also run the parallel login flood (finding 8). Off by
#                    default because it is a load test.
#   --anon-inbound   Also send the unauthenticated inbound webhook probe
#                    (finding 9). Off by default.
#
# Expected results are printed as PASS / FAIL / INFO per check. FAIL means the
# finding reproduces on the live instance; INFO means the check could not run
# without extra input (a cookie jar, an inbox id, ...).
#
# curl follows redirects (-L), so an http BASE_URL on a FORCE_HTTPS instance
# still reaches the app.

set -u

BASE="${BASE_URL:-http://localhost:8081}"
EMAIL=""
COOKIE_JAR=""
COMPOSE_INBOX=""
CURL_OPTS=(-sS -L)
ARGON_FLOOD=0
ANON_INBOUND=0

usage() {
	# Print the leading comment block, stopping at the first non-comment line.
	awk 'NR > 1 { if ($0 ~ /^#/) { sub(/^# ?/, ""); print } else { exit } }' "$0"
	exit "${1:-0}"
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help) usage 0 ;;
	--base)
		BASE="$2"
		shift 2
		;;
	--email)
		EMAIL="$2"
		shift 2
		;;
	--cookie)
		COOKIE_JAR="$2"
		shift 2
		;;
	--compose-inbox)
		COMPOSE_INBOX="$2"
		shift 2
		;;
	--insecure)
		CURL_OPTS+=(-k)
		shift
		;;
	--argon-flood)
		ARGON_FLOOD=1
		shift
		;;
	--anon-inbound)
		ANON_INBOUND=1
		shift
		;;
	*)
		echo "unknown argument: $1" >&2
		usage 2
		;;
	esac
done

BASE="${BASE%/}"
AUTH_OPTS=()
[ -n "$COOKIE_JAR" ] && AUTH_OPTS=(-b "$COOKIE_JAR")

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

pass() { printf '  PASS  %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; FAILED=$((FAILED + 1)); }
info() { printf '  INFO  %s\n' "$1"; }
hdr() { printf '\n== %s\n' "$1"; }
FAILED=0

# curl wrapper: writes the response body to $TMP/body and every response
# header block to $TMP/head, and echoes the final status code. -L is set on
# CURL_OPTS so an instance that 308-redirects http->https (FORCE_HTTPS) is
# followed to the real app.
req() {
	local method="$1"
	shift
	curl "${CURL_OPTS[@]}" -o "$TMP/body" -D "$TMP/head" -w '%{http_code}' \
		-X "$method" "$@" 2>"$TMP/err"
}

# req_nofollow is req without -L: needed for POST probes, where following a
# 303 would turn the POST into a GET and lose the submitted form.
req_nofollow() {
	local method="$1"
	shift
	curl -sS -o "$TMP/body" -D "$TMP/head" -w '%{http_code}' \
		-X "$method" "$@" 2>"$TMP/err"
}

# ---------------------------------------------------------------- finding 4 --
hdr "4  Session/CSRF cookie Secure flag (BASE_URL https + TLS-terminating proxy)"

# GET /login always sets the pre-auth CSRF cookie (mmm_csrf) and redirects.
CSRF_JAR="$TMP/csrf-jar.txt"
code=$(curl "${CURL_OPTS[@]}" -c "$CSRF_JAR" -o "$TMP/body" -D "$TMP/head" -w '%{http_code}' "$BASE/login" 2>"$TMP/err")
setcookie=$(grep -i '^set-cookie: mmm_csrf=' "$TMP/head" || true)
printf '  status: %s\n' "$code"
if [ -z "$setcookie" ]; then
	info "no mmm_csrf cookie on GET /login; check --base points at the app"
else
	printf '  cookie: %s\n' "$(printf '%s' "$setcookie" | tr -d '\r')"
	# A Secure flag is required only when the canonical origin is https. That
	# is BASE_URL, not necessarily the --base we dialled (which may be an
	# http loopback that redirects to the canonical host).
	if printf '%s' "$setcookie" | grep -qi ';\s*Secure'; then
		pass "cookie carries the Secure flag"
	elif [ "${BASE%%://*}" = "https" ]; then
		fail "cookie has NO Secure flag over an https BASE_URL (finding 4)"
	else
		info "cookie is not Secure; dial an https BASE_URL to decide finding 4"
	fi
fi
echo "  note: if you reach this over a TLS-terminating proxy with TRUSTED_PROXIES"
echo "        unset, expect FAIL above (that is finding 4). Behind direct TLS,"
echo "        or with a trusted proxy sending X-Forwarded-Proto, expect PASS."

# ---------------------------------------------------------------- finding 3 --
hdr "3  Open redirect via safeNextPath (control characters in next)"

# POST /login is reached through withPreAuthCSRF, so we must supply a valid
# _csrf equal to the mmm_csrf cookie. We reuse the cookie jar we just filled.
csrf=$(grep -i '^set-cookie: mmm_csrf=' "$TMP/head" | sed -E 's/.*mmm_csrf=([^;]*).*/\1/' | tr -d '\r')
if [ -z "$csrf" ]; then
	info "could not read mmm_csrf; skipping finding 3"
else
	# %09 = TAB. A browser strips it, turning /<TAB>/evil.com into //evil.com.
	code=$(req_nofollow POST "$BASE/login" -b "$CSRF_JAR" \
		--data-urlencode "email=${EMAIL:-nobody@example.com}" \
		--data-urlencode "password=wrong-password-on-purpose" \
		--data-urlencode "_csrf=$csrf" \
		--data-urlencode "next=/$(printf '\t')/evil.com")
	loc=$(grep -i '^location:' "$TMP/head" | tr -d '\r' || true)
	printf '  status: %s  %s\n' "$code" "$loc"
	if printf '%s' "$loc" | grep -qP '[\x00-\x1f\x7f]'; then
		fail "Location contains a raw control character (finding 3)"
	elif printf '%s' "$loc" | grep -qP '(%09|%0A|%0D|%2F%2F)'; then
		fail "Location echoes an encoded control char / scheme-relative next (finding 3)"
	elif printf '%s' "$loc" | grep -qi 'evil\.com'; then
		fail "Location echoes an off-site host (finding 3)"
	elif [ "$code" = "429" ]; then
		info "rate-limited (429); wait a minute and re-run to test finding 3"
	else
		pass "Location is a clean same-origin path"
	fi

	# %0A = newline, %2F%2F = scheme-relative.
	for nxt in '/%0A/evil.com' '//evil.com' 'https://evil.com'; do
		req_nofollow POST "$BASE/login" -b "$CSRF_JAR" \
			--data-urlencode "email=${EMAIL:-nobody@example.com}" \
			--data-urlencode "password=wrong-password-on-purpose" \
			--data-urlencode "_csrf=$csrf" \
			--data-urlencode "next=$nxt" >/dev/null
		loc=$(grep -i '^location:' "$TMP/head" | tr -d '\r' || true)
		if printf '%s' "$loc" | grep -qi 'evil\.com'; then
			fail "next='$nxt' produced an off-site Location: $loc"
		else
			pass "next='$nxt' -> $(printf '%s' "$loc" | cut -d' ' -f2)"
		fi
	done

	echo "  note: on the bad-password path the login page re-renders and echoes"
	echo "        next= in its own redirect, which is what the FAIL above shows."
	echo "        To see the POST-login Location, sign in successfully with the"
	echo "        same crafted next and read the Location of the 303."
fi

# ---------------------------------------------------------------- finding 5 --
hdr "5  CSRF token appended to form action URLs"

if [ -z "$COMPOSE_INBOX" ]; then
	info "no --compose-inbox given; skipping (pass an inbox id and --cookie)"
elif [ -z "$COOKIE_JAR" ]; then
	info "no --cookie given; skipping (need a signed-in session)"
else
	req GET "$BASE/ui/inboxes/$COMPOSE_INBOX/compose" "${AUTH_OPTS[@]}" >/dev/null
	actions=$(grep -oE 'action="[^"]*"' "$TMP/body" || true)
	if [ -z "$actions" ]; then
		info "no form action found; is the session valid and the inbox id right?"
	else
		if printf '%s' "$actions" | grep -q '_csrf='; then
			fail "a form action carries ?_csrf= (finding 5):"
			printf '%s\n' "$actions" | grep '_csrf=' | sed 's/^/        /'
		else
			pass "no form action carries a CSRF token in the URL"
		fi
	fi
fi

# ---------------------------------------------------------------- finding 6 --
hdr "6  Flash store: unauthenticated compose body is parsed and stored"

# POST /ui/inboxes/{id}/send with NO session cookie. expiredSession ->
# preserveComposeOnExpiry parses the multipart body before auth/CSRF.
if [ -z "$COMPOSE_INBOX" ]; then
	info "no --compose-inbox given; skipping"
else
	# 2 MiB of filler, to make the RAM cost of the probe visible.
	head -c 2097152 /dev/zero >"$TMP/fill.bin"
	req_nofollow POST "$BASE/ui/inboxes/$COMPOSE_INBOX/send" \
		-F 'to=a@example.com' -F 'subject=probe' -F 'text=probe body' \
		-F "attachments=@$TMP/fill.bin;type=application/octet-stream" >/dev/null
	loc=$(grep -i '^location:' "$TMP/head" | tr -d '\r' || true)
	printf '  %s\n' "$loc"
	if printf '%s' "$loc" | grep -q '_flash='; then
		fail "anonymous POST produced a compose flash token in the redirect (finding 6)"
		echo "        the 2 MiB body was parsed and stored before authentication"
	else
		pass "anonymous POST did not create a compose flash"
	fi
fi

# ---------------------------------------------------------------- finding 7 --
hdr "7  Login throttle: per-IP only; IPv6 /64 rotation; no per-account limit"

if [ -z "$EMAIL" ]; then
	info "no --email given; skipping the per-account throttle probe"
else
	# Ten wrong-password attempts for one address. If a per-account limit
	# existed, later attempts would be refused with 429 regardless of source.
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		req_nofollow POST "$BASE/login" -b "$CSRF_JAR" \
			--data-urlencode "email=$EMAIL" \
			--data-urlencode "password=wrong-password-on-purpose" \
			--data-urlencode "_csrf=$csrf" >/dev/null
	done
	last=$(req_nofollow POST "$BASE/login" -b "$CSRF_JAR" \
		--data-urlencode "email=$EMAIL" \
		--data-urlencode "password=wrong-password-on-purpose" \
		--data-urlencode "_csrf=$csrf")
	echo "  status of the 11th attempt: $last"
	if [ "$last" != "429" ]; then
		fail "no per-account throttle: 11 failed logins for $EMAIL all reached the app"
	fi
	echo "  note: the per-IP limiter (LOGIN_LIMIT_PER_MINUTE, default 10) may"
	echo "        itself return 429 here, which masks the missing account limit."
	echo "        To isolate it, run from several source addresses, or drive"
	echo "        loginPost directly in a unit test instead."
fi

# ---------------------------------------------------------------- finding 8 --
hdr "8  Argon2 admission: 4 slots, blocking acquire, no timeout"

if [ "$ARGON_FLOOD" -eq 0 ]; then
	info "flood skipped (pass --argon-flood to run it; it is a load test)"
else
	echo "  firing 40 parallel wrong-password logins..."
	start=$(date +%s%N)
	for i in $(seq 1 40); do
		curl "${CURL_OPTS[@]}" -o /dev/null -s -b "$CSRF_JAR" \
			-X POST "$BASE/login" \
			--data-urlencode "email=flood-$i@example.com" \
			--data-urlencode "password=wrong-password-on-purpose" \
			--data-urlencode "_csrf=$csrf" &
	done
	wait
	end=$(date +%s%N)
	echo "  elapsed: $(((end - start) / 1000000)) ms"
	echo "  note: if every request waits for the 4 Argon2 slots, the last ones"
	echo "        finish after roughly (40/4) x one-hash-time. A context-aware"
	echo "        acquire with a timeout would return 503 for the tail instead."
fi

# ---------------------------------------------------------------- finding 9 --
hdr "9  Inbound webhook semaphore held before the signature check"

if [ "$ANON_INBOUND" -eq 0 ]; then
	info "inbound probe skipped (pass --anon-inbound to send it)"
else
	# Nobody expects this to be accepted; we only want to see whether the
	# server takes the shared inbound slot before validating the signature.
	code=$(req POST "$BASE/internal/ingest/mailgun" \
		-H 'Content-Type: application/x-www-form-urlencoded' \
		--data 'signature=x&timestamp=1&token=y&sender=a@b.com&recipient=c@example.com')
	echo "  status: $code (401/403 = signature rejected, which is expected)"
	echo "  note: the shared slot is taken at api.go:2825 BEFORE the transport"
	echo "        verifies the signature. Watch the server's inbound throughput"
	echo "        under parallel anonymous posts to observe head-of-line blocking."
fi

# ------------------------------------------------------------------ summary --
printf '\n== summary\n'
if [ "$FAILED" -eq 0 ]; then
	echo "  no finding reproduced in the checks that ran."
else
	echo "  $FAILED check(s) reproduced a finding (see FAIL lines above)."
fi
echo "  INFO lines mean a check needed extra input (cookie jar, inbox id, ...)."
exit 0
