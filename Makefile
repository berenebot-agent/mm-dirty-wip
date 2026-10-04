.PHONY: build test test-all smoke vet fmt-tests run fmt dist docs changelog dialmx mx-fixture
build:
	./mailmoose-go.sh build ./cmd/server
# dialmx builds the standalone Dial MX receiver (see docs/DIALMX.md).
dialmx:
	./mailmoose-go.sh build -buildvcs=false ./dialmx/cmd/receiver
# mx-fixture builds the receiver binary the launcher RSS test measures. The
# test skips when it is absent, so this is opt-in (Docker root build output
# lands owned by root; harmless, it is gitignored).
mx-fixture:
	./mailmoose-go.sh build -buildvcs=false -o tests/fixtures/mailmoose-mx ./dialmx/cmd/receiver
# Standard commands go through tests/run.sh so every run is captured under
# tests/logs/runs/<run-id>/ (full logs, timings, metadata) and never needs a
# re-run to dig deeper. See docs/TESTING.md.
test:
	./tests/run.sh --unit
# test-all adds the slow gated (smoke) tests.
test-all:
	./tests/run.sh --unit --smoke
smoke:
	./tests/run.sh --smoke
vet:
	./tests/run.sh --vet
# fmt-tests is the CI formatting gate (gofmt -l; fails on output). `fmt` below
# rewrites files instead, so they stay separate.
fmt-tests:
	./tests/run.sh --fmt
run:
	./mailmoose-go.sh run ./cmd/server
fmt:
	./mailmoose-go.sh gofmt -w cmd internal dialmx tests
# docs regenerates the checked-in API reference. The Go command runs in a root
# container, so it writes to stdout and the shell redirect keeps the file owned
# by the host user.
docs:
	./mailmoose-go.sh run ./cmd/docsgen > docs/API-REFERENCE.md
# changelog syncs the embedded copy served at /changelog with the repository
# root CHANGELOG.md. A test fails when the two drift.
changelog:
	cp CHANGELOG.md internal/httpapp/assets/CHANGELOG.md
# dist builds a review/export archive from tracked files only. Using git archive
# (rather than zip) guarantees gitignored paths such as .env and data/ can never
# be bundled, even though they exist in the working tree.
dist:
	git archive --format=zip --output=repo.zip HEAD
