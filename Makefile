.PHONY: vet lint test sandbox-test

vet:
	go vet ./...

lint:
	golangci-lint run

test:
	go test ./...

# Needs Docker. Builds the image, checks how it starts, then runs
# sandbox/test.sh inside it. The script runs as root only so it can act as
# each user. The image itself never runs as root.
IMAGE := issuedog:sandbox-test
sandbox-test:
	docker build -t $(IMAGE) .
	@user=$$(docker image inspect --format '{{.Config.User}}' $(IMAGE)); \
	if [ "$$user" = issuedog ]; then echo "PASS image user"; else echo "FAIL image user: want 'issuedog', got '$$user'"; exit 1; fi
	@if docker run --rm --cap-add NET_ADMIN $(IMAGE) --version >/dev/null; then echo "PASS the image starts the daemon"; else echo "FAIL the image starts the daemon"; exit 1; fi
	@if docker run --rm $(IMAGE) --version >/dev/null 2>&1; then echo "FAIL the image starts without NET_ADMIN"; exit 1; else echo "PASS the image refuses to start without NET_ADMIN"; fi
	docker run --rm --user root --cap-add NET_ADMIN --entrypoint /bin/sh -v "$(CURDIR)/sandbox/test.sh:/test.sh:ro" $(IMAGE) /test.sh
