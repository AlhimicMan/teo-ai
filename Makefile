.PHONY: build test check dist release key

build:
	go build -trimpath -o bin/teo-ai ./cmd/teo-ai
	go build -trimpath -o bin/teo-ai-gateway ./cmd/teo-ai-gateway

test:
	go test -race ./...

check:
	go vet ./...

dist:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o dist/teo-ai-linux-amd64 ./cmd/teo-ai
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o dist/teo-ai-linux-arm64 ./cmd/teo-ai
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o dist/teo-ai-darwin-arm64 ./cmd/teo-ai
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o dist/teo-ai-darwin-amd64 ./cmd/teo-ai

release:
	bash scripts/package-release.sh

key:
	@mkdir -p secrets
	@chmod 0700 secrets
	@umask 077; test ! -e secrets/gateway-key && openssl rand -hex 32 > secrets/gateway-key || { echo 'Key already exists or generation failed; refusing to overwrite'; exit 1; }
	@chmod 0444 secrets/gateway-key
	@echo 'Created secrets/gateway-key (parent directory must remain private).'
