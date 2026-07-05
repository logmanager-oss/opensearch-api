BINARY := osapi
PKG := ./cmd/osapi
SPEC_VERSION := v0.2.0
SPEC_URL := https://github.com/opensearch-project/opensearch-api-specification/releases/download/$(SPEC_VERSION)

.PHONY: build test lint generate tidy fmt clean update-spec

build:
	go build -o $(BINARY) $(PKG)

test:
	go test ./...

lint:
	golangci-lint run

generate:
	go generate ./...

# Bump the spec: set SPEC_VERSION, run `make update-spec`, review the paths_gen.go
# diff, then update the commit SHA in NOTICE.
update-spec:
	curl -fsSL -o internal/apispec/spec/opensearch-openapi.yaml $(SPEC_URL)/opensearch-openapi.yaml
	curl -fsSL -o internal/apispec/spec/LICENSE.txt $(SPEC_URL)/LICENSE.txt
	go generate ./...

tidy:
	go mod tidy

fmt:
	golangci-lint fmt

clean:
	rm -f $(BINARY)
	go clean
