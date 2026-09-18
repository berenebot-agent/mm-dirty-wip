.PHONY: build test run fmt vet dist docs
build:
	./mailmoose-go.sh build ./cmd/server
test:
	./mailmoose-go.sh test -race -count=1 ./...
run:
	./mailmoose-go.sh run ./cmd/server
fmt:
	./mailmoose-go.sh gofmt -w cmd internal
vet:
	./mailmoose-go.sh vet ./...
# docs regenerates the checked-in API reference. The Go command runs in a root
# container, so it writes to stdout and the shell redirect keeps the file owned
# by the host user.
docs:
	./mailmoose-go.sh run ./cmd/docsgen > docs/API-REFERENCE.md
# dist builds a review/export archive from tracked files only. Using git archive
# (rather than zip) guarantees gitignored paths such as .env and data/ can never
# be bundled, even though they exist in the working tree.
dist:
	git archive --format=zip --output=repo.zip HEAD
