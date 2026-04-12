BINARY := maquinista
BUILD_DIR := ./cmd/maquinista
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: build test clean vet

build:
	go build $(LDFLAGS) -o $(BINARY) $(BUILD_DIR)

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f $(BINARY)
