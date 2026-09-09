.PHONY: build test run fmt vet
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
