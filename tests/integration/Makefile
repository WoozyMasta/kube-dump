GO ?= go

KIND_VERSION  ?= 0.33.0
KIND_DIR      ?= .tools
KIND_OS       ?= $(shell $(GO) env GOOS)
KIND_ARCH     ?= $(shell $(GO) env GOARCH)

INTEGRATION_RUNTIME  ?= docker
INTEGRATION_KEEP     ?= 0
INTEGRATION_TIMEOUT  ?= 15m

KIND_SUFFIX :=
ifeq ($(KIND_OS),windows)
KIND_SUFFIX := .exe
endif

KIND_LOCAL_REL   := $(KIND_DIR)/kind-$(KIND_VERSION)-$(KIND_OS)-$(KIND_ARCH)$(KIND_SUFFIX)
KIND_LOCAL       := $(abspath $(KIND_LOCAL_REL))
KIND_PROJECT_URL ?= https://github.com/kubernetes-sigs
KIND_URL         ?= $(KIND_PROJECT_URL)/kind/releases/download/v$(KIND_VERSION)/kind-$(KIND_OS)-$(KIND_ARCH)

ifeq ($(origin KIND),undefined)
KIND = $(if $(wildcard $(KIND_LOCAL)),$(KIND_LOCAL),kind)
endif

.PHONY: integration kind-install

integration:
	@echo "integration: runtime=$(INTEGRATION_RUNTIME) kind=$(KIND) timeout=$(INTEGRATION_TIMEOUT)"
	@echo "integration: running tests"
	@KUBE_DUMP_KEEP_INTEGRATION_ENV=$(INTEGRATION_KEEP) \
	KUBE_DUMP_INTEGRATION_KIND="$(KIND)" \
	KUBE_DUMP_INTEGRATION_RUNTIME=$(INTEGRATION_RUNTIME) \
	$(GO) test -tags=integration -count=1 -timeout=$(INTEGRATION_TIMEOUT) -v ./...
	@echo "integration: passed"

kind-install: $(KIND_LOCAL)

$(KIND_LOCAL):
	@mkdir -p "$(KIND_DIR)"
	@command -v curl >/dev/null 2>&1 || { \
		echo "kind-install requires curl in PATH" >&2; \
		exit 1; \
	}
	@echo "Downloading kind $(KIND_VERSION) for $(KIND_OS)/$(KIND_ARCH)"
	@curl --fail --location --retry 3 --output "$@" "$(KIND_URL)"
	@chmod +x "$@"
