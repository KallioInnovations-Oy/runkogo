.PHONY: all test vet build clean example

# Default: run all checks
all: vet test build

# Run go vet on framework and example
vet:
	@echo "=== go vet ==="
	go vet ./...
	cd example && go vet ./...

# Run all tests with verbose output
test:
	@echo "=== go test ==="
	go test -v -count=1 -race ./...

# Run tests with coverage report
cover:
	@echo "=== coverage ==="
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out
	@rm -f coverage.out

# Build the example binary
build:
	@echo "=== build ==="
	cd example && go build -o ../bin/runko-example .
	@echo "Binary: bin/runko-example"

# Build for Linux (for Docker/Hetzner deployment)
build-linux:
	@echo "=== build-linux ==="
	cd example && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ../bin/runko-example-linux .
	@echo "Binary: bin/runko-example-linux"

# Run the example locally
run: build
	@echo "=== run ==="
	PORT=19100 LOG_LEVEL=debug ./bin/runko-example

# Clean build artifacts
clean:
	rm -rf bin/ coverage.out

# Quick check before commit
precommit: vet test
	@echo ""
	@echo "All checks passed."
