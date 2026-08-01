BINARY := osapi
PKG := ./cmd/osapi
SPEC_VERSION := v0.2.0
SPEC_URL := https://github.com/opensearch-project/opensearch-api-specification/releases/download/$(SPEC_VERSION)

E2E_COMPOSE := docker compose -f e2e/docker-compose.yml
E2E_OPENSEARCH_PORT ?= 19200
export E2E_OPENSEARCH_PORT
E2E_URL := https://127.0.0.1:$(E2E_OPENSEARCH_PORT)
E2E_GOTEST := E2E_OPENSEARCH_URL=$(E2E_URL) go test -tags e2e -count=1

.PHONY: build test lint generate tidy fmt clean update-spec e2e-up e2e-down e2e e2e-test

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

# e2e-up always tears down first: a fresh stack means certs on disk always
# match the running container.
e2e-up:
	$(E2E_COMPOSE) down -v
	sh e2e/gen-certs.sh e2e/certs
	$(E2E_COMPOSE) up --wait

e2e-down:
	$(E2E_COMPOSE) down -v

# Full run: fresh stack, test, guaranteed teardown; test exit code propagates.
# Container logs are dumped on any failure to keep CI runs debuggable.
e2e: build
	$(MAKE) e2e-up || { $(E2E_COMPOSE) logs opensearch; $(E2E_COMPOSE) down -v; exit 1; }
	$(E2E_GOTEST) ./e2e/; status=$$?; \
		if [ $$status -ne 0 ]; then $(E2E_COMPOSE) logs opensearch; fi; \
		$(E2E_COMPOSE) down -v; exit $$status

e2e-test: build
	$(E2E_GOTEST) -v ./e2e/
