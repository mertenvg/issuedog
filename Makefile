.PHONY: vet lint test

vet:
	go vet ./...

lint:
	golangci-lint run

test:
	go test ./...
