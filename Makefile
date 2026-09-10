.PHONY: build test run fmt vet dist
build:
	./gatehouse-go.sh build ./cmd/server
test:
	./gatehouse-go.sh test -race -count=1 ./...
run:
	./gatehouse-go.sh run ./cmd/server
fmt:
	./gatehouse-go.sh gofmt -w cmd internal
vet:
	./gatehouse-go.sh vet ./...
# dist builds a review/export archive from tracked files only. Using git archive
# (rather than zip) guarantees gitignored paths such as .env and data/ can never
# be bundled, even though they exist in the working tree.
dist:
	git archive --format=zip --output=repo.zip HEAD
