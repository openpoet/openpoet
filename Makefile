.PHONY: all build run clean test deps vendor-js setup format

# Variables
BINARY_NAME=openpoet
BUILD_DIR=build
MAIN_PATH=./cmd/openpoet

# Default target
all: deps vendor-js build

# Build the binary
build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	go build -ldflags "-X main.BuildVersion=$$(git rev-parse --short HEAD) -X main.DefaultRelayURL=$(RELAY_URL)" -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN_PATH)
	@echo "Built: $(BUILD_DIR)/$(BINARY_NAME)"

# Build for multiple platforms
build-all: deps vendor-js
	@echo "Building for multiple platforms..."
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 $(MAIN_PATH)
	GOOS=darwin GOARCH=arm64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 $(MAIN_PATH)
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 $(MAIN_PATH)
	GOOS=linux GOARCH=arm64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 $(MAIN_PATH)
	@echo "Build complete!"

# Run the application
run: build
	@echo "Running $(BINARY_NAME)..."
	./$(BUILD_DIR)/$(BINARY_NAME)

# Run in development mode
dev:
	go run $(MAIN_PATH)

# Download dependencies
deps:
	@echo "Downloading dependencies..."
	go mod download
	go mod tidy

# Download vendor JavaScript libraries
vendor-js:
	@echo "Setting up vendor JavaScript libraries..."
	@mkdir -p web/static/vendor
	@if [ ! -f web/static/vendor/xterm.js ]; then \
		echo "Downloading xterm.js..."; \
		curl -sL https://cdn.jsdelivr.net/npm/xterm@5.3.0/lib/xterm.min.js -o web/static/vendor/xterm.js; \
		curl -sL https://cdn.jsdelivr.net/npm/xterm@5.3.0/css/xterm.css -o web/static/vendor/xterm.css; \
		curl -sL https://cdn.jsdelivr.net/npm/xterm-addon-fit@0.8.0/lib/xterm-addon-fit.min.js -o web/static/vendor/xterm-addon-fit.js; \
		curl -sL https://cdn.jsdelivr.net/npm/xterm-addon-web-links@0.9.0/lib/xterm-addon-web-links.min.js -o web/static/vendor/xterm-addon-web-links.js; \
		echo "Vendor libraries downloaded."; \
	else \
		echo "Vendor libraries already exist."; \
	fi

# Run tests
test:
	go test -v ./...

# Run tests with coverage
test-coverage:
	go test -v -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

# Clean build artifacts
clean:
	@echo "Cleaning..."
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html
	rm -f openpoet.db

# Format code
fmt:
	go fmt ./...

# Format Go files and stage changes
format:
	gofmt -w .
	git add -u

# Lint code
lint:
	@if command -v golangci-lint &> /dev/null; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed"; \
	fi

# Generate PWA icons (requires ImageMagick)
icons:
	@if ! command -v magick &> /dev/null; then \
		echo "Error: ImageMagick is not installed."; \
		echo "  Install with: brew install imagemagick"; \
		exit 1; \
	fi
	@if [ ! -f web/static/favicon.svg ]; then \
		echo "Error: Source file web/static/favicon.svg not found."; \
		exit 1; \
	fi
	@echo "Generating PWA icons from web/static/favicon.svg..."
	@magick -background none -size 192x192 web/static/favicon.svg web/static/icon-192.png && \
		echo "  Created web/static/icon-192.png" || \
		{ echo "Error: Failed to generate icon-192.png"; exit 1; }
	@magick -background none -size 512x512 web/static/favicon.svg web/static/icon-512.png && \
		echo "  Created web/static/icon-512.png" || \
		{ echo "Error: Failed to generate icon-512.png"; exit 1; }
	@echo "Done."

# Setup git hooks
setup:
	git config core.hooksPath .githooks
	@echo "Git hooks configured."

# Install development tools
tools:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

# Show help
help:
	@echo "OpenPoet Makefile"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  all          Build with dependencies and vendor JS (default)"
	@echo "  build        Build the binary"
	@echo "  build-all    Build for multiple platforms"
	@echo "  run          Build and run the application"
	@echo "  dev          Run in development mode"
	@echo "  deps         Download Go dependencies"
	@echo "  vendor-js    Download vendor JavaScript libraries"
	@echo "  test         Run tests"
	@echo "  test-coverage Run tests with coverage"
	@echo "  clean        Remove build artifacts"
	@echo "  fmt          Format code (go fmt)"
	@echo "  format       Format Go files and stage changes"
	@echo "  lint         Lint code"
	@echo "  icons        Generate PWA icons"
	@echo "  setup        Configure git hooks"
	@echo "  tools        Install development tools"
	@echo "  help         Show this help"
