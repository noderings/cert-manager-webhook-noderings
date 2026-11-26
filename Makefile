IMAGE ?= ghcr.io/noderings/cert-manager-webhook-noderings
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
ENVTEST_K8S_VERSION ?= 1.36.2
ENVTEST_DIR ?= $(CURDIR)/_bin/envtest
SETUP_ENVTEST = go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.22

.PHONY: build test lint image test-conformance e2e

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/webhook ./cmd/webhook

test:
	go test -race ./...

lint:
	golangci-lint run
	golangci-lint run --build-tags conformance
	helm lint deploy/cert-manager-webhook-noderings

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

# cert-manager's DNS-01 conformance suite against the real NodeRings API.
# Needs NODERINGS_TOKEN; creates and deletes a throwaway zone.
test-conformance:
	ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_DIR) -p path)" && \
	TEST_ASSET_ETCD="$$ASSETS/etcd" TEST_ASSET_KUBE_APISERVER="$$ASSETS/kube-apiserver" TEST_ASSET_KUBECTL="$$ASSETS/kubectl" \
	go test -tags conformance -count=1 -timeout 15m -v ./test/conformance/

# Issues a certificate in a kind cluster through the real NodeRings API. See hack/e2e.sh.
e2e:
	hack/e2e.sh
