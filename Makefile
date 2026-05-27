.PHONY: build test test-blackbox run lint docker-build clean

# Fast checks — no docker required.
test:
	go test ./...

# Full conformance suite — requires a running docker daemon. Uses official
# language client images via testcontainers-go. See docs/blackbox-testing.md.
test-blackbox:
	go test -tags=blackbox -timeout=10m ./tests/blackbox/...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o ./bin/pkgmirror ./cmd/pkgmirror

run:
	go run ./cmd/pkgmirror

docker-build:
	docker build -t pkgmirror:dev .

lint:
	go vet ./...

clean:
	rm -rf ./bin ./data
