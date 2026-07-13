.PHONY: all build fmt vet test test-race check clean

all: check build

build:
	mkdir -p bin
	go build -o bin/termcall ./cmd/termcall
	go build -o bin/termcall-signald ./cmd/termcall-signald

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
