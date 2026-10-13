BINARY := bin/sandboxd
CLI := bin/sbxctl
VERSION ?= 1.1.8

.PHONY: build test test-race test-int test-e2e test-e2e-race test-e2e-destructive \
        test-commercial bench scenario scenario-flow fmt vet clean

build: fmt
	go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) ./cmd/sandboxd
	go build -trimpath -ldflags "-s -w" -o $(CLI) ./cmd/sbxctl

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './integrations/*')

vet:
	go vet ./...

test:
	go test -race -count=1 ./internal/... 

test-int:
	go test -race -count=1 -v ./internal/runtime/oci/ ./internal/store/

# The end-to-end suites live in ./tests as Go tests over a shared harness
# (daemon lifecycle, typed client, tenant and sandbox fixtures). They are the
# acceptance gate, so this target sets SBX_REQUIRE_DOCKER=1: a missing
# container runtime fails the run instead of skipping every scenario, because
# a suite that silently skips everything is indistinguishable from one that
# passes. For a laptop without a runtime, run `go test ./tests/` directly —
# the suites skip themselves there.
test-e2e: build
	SBX_REQUIRE_DOCKER=1 go test -count=1 -timeout 60m ./tests/
	@node tests/pi-sbx-harness.mjs

# The same suites with the daemon under test built by -race, so the race
# detector instruments the daemon's goroutines rather than only the harness's.
test-e2e-race: build
	SBX_IT_RACE=1 SBX_REQUIRE_DOCKER=1 go test -count=1 -timeout 90m ./tests/

# The two scenarios that disrupt every other workload on the machine (cycling
# the container runtime) are gated separately; nothing else needs this flag.
test-e2e-destructive: build
	SBX_REQUIRE_DOCKER=1 SBX_IT_DESTRUCTIVE=1 go test -count=1 -timeout 90m \
		-run 'TestBackendOutageIsDisclosedAndRecovers' ./tests/

# The commercial-standard suite: the five axes a buyer's technical review asks
# about separately — 启动实效 (startup), 高并发 (concurrency), 可用性
# (availability under sustained load), 弹性 (elasticity), 响应性 (latency).
# Security is not in this target because it has its own suites.
#
# Two scenarios in it are slow by construction: idle and TTL reclamation have a
# 60s floor (domain.SandboxSpec.Validate), so this target takes about four
# minutes. That is the honest cost of testing expiry through the API instead of
# through a unit test with a fake clock.
test-commercial: build
	SBX_REQUIRE_DOCKER=1 go test -count=1 -timeout 30m -v -run 'TestStartupIsMeasuredNotJustTimedOut|TestAdmissionIsExactUnderConcurrency|TestLatencyIsBoundedByTheCheapestRequest|TestControlPlaneLatencyIsFlatInFleetSize|TestServiceStaysAvailableUnderSustainedLoad|TestOneTenantsOverloadIsNotAnotherTenantsLatency|TestElasticityIdleReclaimIsActivityDriven|TestElasticityTTLReturnsQuotaWithoutAClientCall' ./tests/

# The reproducible cold-start/throughput report (docs/ROADMAP.md §P0-2). It
# measures — it does not assert an absolute budget — and writes a JSON summary
# to SBX_BENCH_OUT so a buyer or a future engineer can re-run and diff.
bench: build
	SBX_REQUIRE_DOCKER=1 SBX_BENCH_OUT=docs/bench-summary.json go test -count=1 -timeout 20m -v -run TestBenchReport ./tests/

# The enterprise walkthrough: one scenario, end to end, against a live daemon,
# writing a machine-readable trace of every real call it makes. It is not a
# substitute for the Go suites — it is the buyer-facing rehearsal, and the
# source of docs/scenario-flow.html.
scenario: build
	tests/e2e-enterprise-scenario.sh

# Regenerate the dynamic flow diagram FROM the last scenario trace. The
# diagram is derived, never hand-written: to change the flow, re-run the
# scenario rather than editing the picture.
scenario-flow: scenario
	python3 tests/render-scenario-flow.py

clean:
	rm -rf bin data
