.PHONY: build run test fmt lint

build:
	go build ./...

run:
	go run ./cmd/bot -config ./configs/config.local.json

test:
	go test ./...

fmt:
	gofmt -w .

lint:
	go vet ./...
