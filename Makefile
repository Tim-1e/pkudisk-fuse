GO ?= go

.PHONY: build test check clean
build:
	mkdir -p bin
	$(GO) build -trimpath -o bin/pkudisk-fuse ./cmd/pkudisk-fuse
test:
	$(GO) test -race ./...
check:
	$(GO) vet ./...
clean:
	rm -rf bin
