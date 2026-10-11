.PHONY: vet lint test sandbox-test

vet:
	go vet ./...

lint:
	golangci-lint run

test:
	go test ./...

# Needs Docker. Builds the image and runs sandbox/test.sh inside it as root.
sandbox-test:
	docker build -t issuedog:sandbox-test .
	docker run --rm --entrypoint /bin/sh -v "$(CURDIR)/sandbox/test.sh:/test.sh:ro" issuedog:sandbox-test /test.sh
