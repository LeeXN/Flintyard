GO ?= go
VERSION := $(shell sed -n 's/^const Version = "\(.*\)"$$/\1/p' internal/version/version.go)
export GOTOOLCHAIN = local

.PHONY: test race vet fmt build check package
fmt:
	$(GO) fmt ./...
vet:
	$(GO) vet ./...
test:
	$(GO) test ./...
race:
	$(GO) test -race ./...
build:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath ./...
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -o bin/flintyard-linux-amd64 ./cmd/flintyard
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath ./...
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -o bin/flintyard-linux-arm64 ./cmd/flintyard
check: vet test race build
package: build
	mkdir -p dist
	tar -czf dist/flintyard-v$(VERSION)-linux-amd64.tar.gz -C bin flintyard-linux-amd64
	tar -czf dist/flintyard-v$(VERSION)-linux-arm64.tar.gz -C bin flintyard-linux-arm64
	cd dist && sha256sum *.tar.gz > SHA256SUMS
