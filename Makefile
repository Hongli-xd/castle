BINARY := bin/sandboxd
CLI := bin/sbxctl
VERSION ?= 1.0.0

.PHONY: build test test-race test-int test-e2e fmt vet clean

build: fmt
	go build -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) ./cmd/sandboxd
	go build -ldflags "-s -w" -o $(CLI) ./cmd/sbxctl

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './integrations/*')

vet:
	go vet ./...

test:
	go test -race -count=1 ./internal/... 

test-int:
	go test -race -count=1 -v ./internal/runtime/oci/ ./internal/store/

test-e2e: build
	./tests/chaos.sh && ./tests/load.sh && ./tests/security.sh \
		&& ./tests/malicious.sh && ./tests/multiuser.sh && ./tests/disaster.sh \
		&& node tests/pi-sbx-harness.mjs

clean:
	rm -rf bin data
