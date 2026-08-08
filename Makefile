export GOTOOLCHAIN := local

build:
	go build ./...

test:
	go test -race ./...

vet:
	go vet ./...

align:
	go run ./cmd/align -days 30
