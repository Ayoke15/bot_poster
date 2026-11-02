.PHONY: build test integration fmt vet
build:
	go build -trimpath -o bin/bot ./cmd/bot
test:
	go test -race ./...
integration:
	@test -n "$$TEST_DATABASE_URL" || (echo 'Set TEST_DATABASE_URL to an isolated PostgreSQL database'; exit 1)
	go test -race -count=1 ./...
fmt:
	gofmt -w cmd internal
vet:
	go vet ./...
