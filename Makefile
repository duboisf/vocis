APP := vocis

.PHONY: build test fmt tidy

build:
	mkdir -p bin
	go build -o bin/$(APP) ./cmd/vocis

test:
	go test ./...

fmt:
	gofmt -w ./cmd ./internal

tidy:
	go mod tidy

