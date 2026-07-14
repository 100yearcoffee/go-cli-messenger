.PHONY: all build fmt vet test test-race check clean

all: check build

build:
	./scripts/build

fmt:
	gofmt -w $$(find cmd internal test -name '*.go' -type f 2>/dev/null)

vet:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

check: fmt vet test

clean:
	rm -rf bin
