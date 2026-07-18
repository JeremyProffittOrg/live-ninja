# live-ninja build tooling.
#
# Dev machine is Windows; CI is ubuntu-latest. Keep every target POSIX-sh
# compatible (no PowerShell, no bash-only bashisms) so this Makefile behaves
# identically under GNU Make on both.

SHELL := /bin/sh

# Logical function name -> ./cmd/<name> directory (per shared spec).
FUNCTIONS := web authorizer realtime-broker iot-ingest usage-rollup email-dispatch deliverables-zipper shadow-ingest topics-extract account-purge

.PHONY: all build test vet lint clean

all: build

# Compiles each Lambda function to .build/<fn>/bootstrap for provided.al2023 / arm64,
# matching the Architectures: [arm64] declared in template.yaml. Flip the GOARCH and
# the template's Architectures together if this ever changes.
build:
	@set -e; \
	for fn in $(FUNCTIONS); do \
		echo "==> building $$fn (./cmd/$$fn -> .build/$$fn/bootstrap)"; \
		mkdir -p .build/$$fn; \
		GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags lambda.norpc -trimpath -ldflags "-s -w -X 'github.com/JeremyProffittOrg/live-ninja/internal/webapp.BuildVersion=$${BUILD_VERSION:-$$(cat VERSION 2>/dev/null || echo 0.0.0)+$$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}'" -o .build/$$fn/bootstrap ./cmd/$$fn; \
	done

test:
	go vet ./...
	go test ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf .build .aws-sam
