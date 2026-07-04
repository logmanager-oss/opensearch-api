BINARY := osapi
PKG := ./cmd/osapi

.PHONY: build test lint generate tidy fmt clean

build:
	go build -o $(BINARY) $(PKG)

test:
	go test ./...

lint:
	golangci-lint run

generate:
	go generate ./...

tidy:
	go mod tidy

fmt:
	golangci-lint fmt

clean:
	rm -f $(BINARY)
	go clean
