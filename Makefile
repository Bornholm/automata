.PHONY: fmt build test vet race

fmt:
	gofmt -l -w .

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

race:
	go test -race ./...

include misc/**/*.mk
