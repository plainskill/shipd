VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REGISTRY ?= localhost:5000
IMAGE ?= $(REGISTRY)/atlas/shipd
GO ?= go
DIST ?= dist

.PHONY: help vet build cli image push deploy dist release clean

help:
	@grep -E '^[a-z-]+:.*?##' $(MAKEFILE_LIST) | sed 's/:.*##/\t/' | column -t -s "$$(printf '\t')"

vet: check-dash ## run go vet + dashboard JS syntax check
	$(GO) vet ./...

check-dash: ## extract the embedded dashboard script and syntax-check it
	@command -v node >/dev/null 2>&1 || { echo "check-dash: node not installed, skipping"; exit 0; }
	@python3 -c "import re,sys; s=open('dashboard.html').read(); m=re.search(r'<script>(.*?)</script>', s, re.S); open('/tmp/shipd-dash.js','w').write(m.group(1))"
	@node --check /tmp/shipd-dash.js && echo "check-dash: JS syntax ok"

build: ## build the server binary for this host
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(DIST)/shipd .

cli: ## build the CLI for this host
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(DIST)/shipd-cli ./cli

dist: ## cross-compile release binaries + checksums
	rm -rf $(DIST) && mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(DIST)/shipd-cli-linux-amd64   ./cli
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(DIST)/shipd-cli-linux-arm64   ./cli
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(DIST)/shipd-cli-darwin-arm64  ./cli
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(DIST)/shipd-cli-darwin-amd64  ./cli
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(DIST)/shipd-server-linux-amd64 .
	cd $(DIST) && sha256sum shipd-* > SHA256SUMS
	@ls -la $(DIST)

image: ## build the artifact image (server binary) locally
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

push: image ## build + push the artifact image to the local registry
	docker push $(IMAGE):$(VERSION)
	docker push $(IMAGE):latest

deploy: ## build on pscA, push to the registry, update the running service
	./deploy.sh

clean:
	rm -rf $(DIST)
