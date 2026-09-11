GO                ?= go

LINTER            ?= golangci-lint
LINTER_VERSION    ?= v2.13.2
ALIGNER           ?= betteralign
ALIGNER_VERSION   ?= v0.15.1
VULNCHECK         ?= govulncheck
VULNCHECK_VERSION ?= v1.8.0
SCHEMADOC         ?= schemadoc
SCHEMADOC_VERSION ?= v0.7.1
SCHEMADOC_CONFIG  ?= schemadoc.yaml
WINRES            ?= go-winres
WINRES_VERSION    ?= v0.3.3
BENCHSTAT         ?= benchstat
RUMDL             ?= rumdl

BINARY            ?= kube-dump
OUTPUT_DIR        ?= build
DEPLOY_DIR        ?= deploy
CGO_ENABLED       ?= 0
GOFLAGS           ?= -buildvcs=auto -trimpath
LDFLAGS           ?= -s -w
GOWORK            ?= off

BENCH_COUNT       ?= 6
BENCH_REF         ?= bench_baseline.txt
FUZZ_TIME         ?= 30s
FUZZ_TARGETS      ?= \
	./internal/archive:ReadEntries \
	./internal/archive:ValidateArchivePath \
	./internal/crypto/keyring:DecodeEnvelope \
	./internal/crypto/keyring:EncodeEnvelopeRoundTrip \
	./internal/crypto/siv:DecodePayload \
	./internal/crypto/siv:EncryptDecrypt \
	./internal/images:NormalizeReference \
	./internal/images:ParsePlatform \
	./internal/kube:ResourceSelection \
	./internal/profile:LoadBytes \
	./internal/state/codec:MarshalUnmarshal \
	./internal/state/codec:Unmarshal

MODULE_PATH       ?= $(shell GOWORK=off $(GO) list -m -f '{{.Path}}')
RELEASE_MATRIX    ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
NATIVE_GOOS       := $(shell go env GOOS)
NATIVE_GOARCH     := $(shell go env GOARCH)
BUILD_GOOS        ?= $(NATIVE_GOOS)
BUILD_GOARCH      ?= $(NATIVE_GOARCH)
BUILD_EXTENSION   := $(if $(filter $(BUILD_GOOS),windows),.exe,)
VERSION           := $(shell git describe --tags --abbrev=0 || echo v0.0.0)
COMMIT            := $(shell git rev-parse HEAD || echo unknown)
DATE              := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
CONTAINER_IMAGE   ?=
URL               ?= https://$(MODULE_PATH)
VARS_PATH         ?= $(MODULE_PATH)/internal/version
LDFLAGS_X         := \
	-X '$(VARS_PATH).Version=$(VERSION)' \
	-X '$(VARS_PATH).Commit=$(COMMIT)' \
	-X '$(VARS_PATH).BuildTime=$(DATE)' \
	-X '$(VARS_PATH).URL=$(URL)'

ifneq ($(strip $(CONTAINER_IMAGE)),)
LDFLAGS_X += -X '$(VARS_PATH).ContainerImageOverride=$(CONTAINER_IMAGE)'
endif

RACE ?= 0
ifeq ($(RACE),1)
	EXTRA_BUILD_FLAGS := -race
endif

export GOWORK

.PHONY: clean build compile release

clean:
	rm -rf $(OUTPUT_DIR)

build: clean generate winres compile

compile:
	@mkdir -p $(OUTPUT_DIR)
	@echo ">> build: $(BINARY)$(BUILD_EXTENSION) ($(BUILD_GOOS)/$(BUILD_GOARCH))"
	GOOS=$(BUILD_GOOS) GOARCH=$(BUILD_GOARCH) \
	GOWORK=$(GOWORK) CGO_ENABLED=$(CGO_ENABLED) \
	$(GO) build $(GOFLAGS) -ldflags="$(LDFLAGS) $(LDFLAGS_X)" $(EXTRA_BUILD_FLAGS) \
	-o $(OUTPUT_DIR)/$(BINARY)$(BUILD_EXTENSION) ./cmd/$(BINARY)

release: clean generate winres
	@mkdir -p $(OUTPUT_DIR)
	@set -e; \
	for target in $(RELEASE_MATRIX); do \
		goos=$${target%%/*}; \
		goarch=$${target##*/}; \
		ext=$$( [ $$goos = "windows" ] && echo ".exe" || echo "" ); \
		out="$(OUTPUT_DIR)/$(BINARY)-$${goos}-$${goarch}$$ext"; \
		archive="$(OUTPUT_DIR)/$(BINARY)-$${goos}-$${goarch}.tar.gz"; \
		package_root="$(BINARY)-$(VERSION)"; \
		package_dir="$(OUTPUT_DIR)/$$package_root"; \
		echo ">> build $$out"; \
		GOOS=$$goos GOARCH=$$goarch GOWORK=$(GOWORK) CGO_ENABLED=$(CGO_ENABLED) \
			$(GO) build $(GOFLAGS) -ldflags="$(LDFLAGS) $(LDFLAGS_X)" -o $$out ./cmd/$(BINARY); \
		mkdir -p "$$package_dir/docs/guide" "$$package_dir/deploy"; \
		cp "$$out" "$$package_dir/$(BINARY)$$ext"; \
		cp LICENSE "$$package_dir/LICENSE"; \
		cp pkg/profile/schema/profile.schema.json "$$package_dir/docs/profile.schema.json"; \
		cp docs/example.profile.yaml "$$package_dir/docs/example.profile.yaml"; \
		cp docs/profile.md "$$package_dir/docs/profile.md"; \
		cp docs/cli.md "$$package_dir/docs/cli.md"; \
		cp docs/assets/images/logo-wide.png "$$package_dir/docs/logo-wide.png"; \
		cp -R docs/en/. "$$package_dir/docs/guide/"; \
		cp -R deploy/. "$$package_dir/deploy/"; \
		tar -czf "$$archive" -C "$(OUTPUT_DIR)" "$$package_root"; \
		rm -rf "$$package_dir"; \
	done
	$(MAKE) generate-krew


.PHONY: check ci

check: verify tidy fmt vet vulncheck lint-fix align-fix test test-race version-update
ci: download tools-ci version-verify verify tidy-check fmt-check vet vulncheck lint align test

.PHONY: test test-race integration fuzz

test:
	$(GO) test ./...

test-race:
	CGO_ENABLED=1 $(GO) test -race ./...

integration:
	$(MAKE) -C tests/integration integration

fuzz:
	@set -e; \
	for target in $(FUZZ_TARGETS); do \
		echo "fuzz target: Fuzz$${target##*:} form file $${target%%:*}"; \
		$(GO) test $${target%%:*} -run='^$$' -fuzz='^Fuzz'$${target##*:}'$$' -fuzztime=$(FUZZ_TIME); \
	done

.PHONY: bench bench-fast bench-reset

bench:
	@tmp=$$(mktemp); \
	$(GO) test ./... -run=^$$ -bench 'Benchmark' -benchmem -count=$(BENCH_COUNT) | tee "$$tmp"; \
	if [ -f "$(BENCH_REF)" ]; then \
		$(BENCHSTAT) "$(BENCH_REF)" "$$tmp"; \
	else \
		cp "$$tmp" "$(BENCH_REF)" && echo "Baseline saved to $(BENCH_REF)"; \
	fi; \
	rm -f "$$tmp"

bench-fast:
	$(GO) test ./... -run=^$$ -bench 'Benchmark' -benchmem

bench-reset:
	rm -f "$(BENCH_REF)"

.PHONY: download verify vet tidy tidy-check fmt fmt-check vulncheck lint lint-fix align align-fix

download:
	$(GO) mod download

verify:
	$(GO) mod verify

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

tidy-check:
	@$(GO) mod tidy
	@git diff --stat --exit-code -- go.mod go.sum || ( \
		echo "go mod tidy: repository is not tidy"; \
		exit 1; \
	)

fmt:
	gofmt -w .

fmt-check:
	@files="$$(gofmt -l .)"; \
	if [ -n "$$files" ]; then \
		echo "$$files"; \
		echo "gofmt: files need formatting"; \
		exit 1; \
	fi

vulncheck:
	$(VULNCHECK) ./...

lint:
	$(LINTER) run ./...

lint-fix:
	$(LINTER) run --fix ./...

align:
	$(ALIGNER) ./...

align-fix:
	-$(ALIGNER) -apply ./...
	$(ALIGNER) ./...

.PHONY: generate generate-docs generate-schema generate-krew generate-check

generate: generate-docs generate-schema

generate-docs:
	$(GO) run ./cmd/$(BINARY) docs md docs/cli.md --program-name $(BINARY) \
		--style posix --template table

generate-schema:
	$(SCHEMADOC) build $(SCHEMADOC_CONFIG)

generate-krew:
	@mkdir -p $(OUTPUT_DIR)
	$(GO) run ./cmd/krew-manifest \
		--version "$(VERSION)" \
		--build-dir "$(OUTPUT_DIR)" \
		--output "$(OUTPUT_DIR)/krew.yaml"

generate-check: generate
	@git diff --stat --exit-code -- \
	 pkg/profile/schema/ \
	 docs/ || ( \
	 echo "schema/docs are out of date; run 'make schema' and commit changes"; \
	 exit 1; \
	)

.PHONY: version-update version-verify

version-update:
	$(GO) run ./cmd/update-version --version "$(VERSION)"

version-verify:
	$(GO) run ./cmd/update-version --version "$(VERSION)" --check

.PHONY: winres

winres:
	$(WINRES) make \
	--in winres/manifest.json \
	--arch amd64,arm64 \
	--out ./cmd/$(BINARY)/rsrc \
	--product-version "$(VERSION)" \
	--file-version "$(VERSION)"

.PHONY: tools tools-ci tools-build \
	tool-golangci-lint tool-betteralign tool-govulncheck tool-schemadoc tool-winres tool-benchstat

tools: tool-golangci-lint tool-betteralign tool-govulncheck tool-schemadoc tool-winres tool-benchstat
tools-ci: tool-golangci-lint tool-betteralign tool-govulncheck
tools-build: tool-schemadoc tool-winres

tool-golangci-lint:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(LINTER_VERSION)

tool-betteralign:
	$(GO) install github.com/dkorunic/betteralign/cmd/betteralign@$(ALIGNER_VERSION)

tool-govulncheck:
	$(GO) install golang.org/x/vuln/cmd/govulncheck@$(VULNCHECK_VERSION)

tool-schemadoc:
	$(GO) install github.com/woozymasta/schemadoc/cmd/schemadoc@$(SCHEMADOC_VERSION)

tool-winres:
	$(GO) install github.com/tc-hib/go-winres@$(WINRES_VERSION)

tool-benchstat:
	$(GO) install golang.org/x/perf/cmd/benchstat@latest

.PHONY: release-notes

release-notes:
	@awk '\
	/^<!--/,/^-->/ { next } \
	/^## \[[0-9]+\.[0-9]+\.[0-9]+\]/ { if (found) exit; found=1; next } \
	found { \
		if (/^## \[/) { exit } \
		if (/^$$/) { flush(); print; next } \
		if (/^\* / || /^- /) { flush(); buf=$$0; next } \
		if (/^###/ || /^\[/) { flush(); print; next } \
		sub(/^[ \t]+/, ""); sub(/[ \t]+$$/, ""); \
		if (buf != "") { buf = buf " " $$0 } else { buf = $$0 } \
		next \
	} \
	function flush() { if (buf != "") { print buf; buf = "" } } \
	END { flush() } \
	' CHANGELOG.md

.PHONY: mkdocs-prepare mkdocs-run

mkdocs-prepare:
	docker build -f Dockerfile.docs -t kube-dump:mkdocs-dev .

mkdocs-run: mkdocs-prepare
	docker run --rm -it -p 8000:8000 -v "${PWD}:/docs" \
  kube-dump:mkdocs-dev serve --dev-addr=0.0.0.0:8000

.PHONY: markdown-lint markdown-fix

define run-rumdl
	@if command -v $(RUMDL) &>/dev/null; then \
		$(RUMDL) $(1); \
	else \
		echo "WARN: $(RUMDL) not found; skipping markdown lint."; \
		echo 'WARN: Install it https://github.com/rvben/rumdl'; \
	fi
endef

markdown-lint:
	$(call run-rumdl,check)

markdown-fix:
	$(call run-rumdl,check --fix)
