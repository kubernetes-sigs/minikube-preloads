BIN_DIR := out
BIN := $(BIN_DIR)/minikube-preloads
GOCACHE ?= $(CURDIR)/.cache/go-build
GOMODCACHE ?= $(CURDIR)/.cache/go-mod

# Settings for generating and uploading minikube preloads
WORKDIR ?= $(CURDIR)/out/minikube-work
ARTIFACT_DIR ?= $(CURDIR)/out/artifacts
MINIKUBE_REPO ?= https://github.com/kubernetes/minikube.git
MINIKUBE_REF ?= HEAD
RELEASE_TAG ?= latest
PRELOAD_LIMIT ?= 3
GITHUB_REPOSITORY ?=

.PHONY: build
build:
	@mkdir -p $(BIN_DIR) $(GOCACHE) $(GOMODCACHE)
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go build -o $(BIN) ./cmd/preload-generator

.PHONY: clean-workdir
clean-workdir:
	rm -rf $(WORKDIR)

.PHONY: generate-preloads
generate-preloads: clean-workdir
	@mkdir -p $(WORKDIR) $(ARTIFACT_DIR)
	git clone --filter=blob:none $(MINIKUBE_REPO) $(WORKDIR)
	cd $(WORKDIR) && git fetch origin $(MINIKUBE_REF)
	cd $(WORKDIR) && git checkout FETCH_HEAD
	cd $(WORKDIR) && make update-kubeadm-constants
	cd $(WORKDIR) && make out/minikube out/preload-generator
	cd $(WORKDIR) && out/preload-generator --no-upload --limit $(PRELOAD_LIMIT)
	@mkdir -p $(ARTIFACT_DIR)
	@artifacts=$$(find $(WORKDIR)/out -maxdepth 1 -type f -name '*.lz4' -print); \
	if [ -z "$$artifacts" ]; then \
		echo "No .lz4 artifacts found in $(WORKDIR)/out"; \
	else \
		for f in $$artifacts; do \
			mv "$$f" $(ARTIFACT_DIR)/; \
		done; \
	fi

.PHONY: upload-preloads
upload-preloads:
	@test -n "$(GITHUB_TOKEN)" || (echo "GITHUB_TOKEN must be set for gh release upload" && exit 1)
	@test -d "$(ARTIFACT_DIR)" || (echo "Artifact directory $(ARTIFACT_DIR) not found. Run generate-preloads first." && exit 1)
	@artifacts=$$(find $(ARTIFACT_DIR) -type f); \
	if [ -z "$$artifacts" ]; then \
		echo "No artifacts to upload from $(ARTIFACT_DIR); skipping upload"; \
		exit 0; \
	fi; \
	repo=$(GITHUB_REPOSITORY); \
	if [ -z "$$repo" ]; then \
		repo=$$(gh repo view --json nameWithOwner --jq .nameWithOwner); \
	fi; \
	tag=$(RELEASE_TAG); \
	if [ "$$tag" = "latest" ]; then \
		tag=$$(gh release view --repo "$$repo" --json tagName --jq .tagName); \
	fi; \
	echo "Uploading assets to $$repo release $$tag"; \
	gh release upload --repo "$$repo" --clobber "$$tag" $$artifacts

.PHONY: release-preloads
release-preloads: generate-preloads upload-preloads
