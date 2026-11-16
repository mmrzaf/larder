MODULE := github.com/mmrzaf/larder
# The version comes from the nearest tag; "dev" outside a tagged checkout.
VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null | sed 's/^v//' || echo dev)
COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo none)
# The build date is the commit date, so rebuilding a commit gives the same binary.
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null || echo 0)
DATE ?= $(shell date -u -d @$(SOURCE_DATE_EPOCH) +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)
IMAGE ?= larder
GO ?= go
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)
GOBUILD := CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)"
PLATFORMS := linux/amd64 linux/arm64

.PHONY: build test race vet fmt cover docker ci acceptance release clean

build:
	mkdir -p bin
	$(GOBUILD) -o bin/larder ./cmd/larder

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

cover:
	$(GO) test -cover ./...

docker:
	docker build -f deploy/Dockerfile \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) \
		-t $(IMAGE):$(VERSION) .

ci:
	./scripts/ci.sh

# Real npm, pip, apt and apk clients against real registries. Requires docker, node and
# network access; any skipped or failed check fails the run.
acceptance:
	./scripts/acceptance.sh

# release builds versioned, reproducible archives and checksums in dist/.
release: clean
	mkdir -p dist
	@set -e; for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		name=larder_$(VERSION)_$${os}_$${arch}; \
		mkdir -p dist/$$name; \
		GOOS=$$os GOARCH=$$arch $(GOBUILD) -o dist/$$name/larder ./cmd/larder; \
		cp LICENSE README.md docs/05-configuration/example.yaml dist/$$name/; \
		tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@$(SOURCE_DATE_EPOCH) \
			-C dist -cf - $$name | gzip -n -9 > dist/$$name.tar.gz; \
		rm -r dist/$$name; \
	done
	cd dist && sha256sum *.tar.gz > SHA256SUMS

clean:
	$(RM) -r bin dist coverage.out
