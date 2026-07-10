# Variables
APP_NAME = indexer

.PHONY: all build clean run build-all

all: build

# Build for the current native operating system
build:
	@echo "Building $(APP_NAME)..."
	go build -o $(APP_NAME) ./cmd/agent

# Cross-compile for both Linux and Windows
build-all:
	@echo "Building for Linux (amd64)..."
	GOOS=linux GOARCH=amd64 go build -o $(APP_NAME)-linux-amd64 ./cmd/agent
	@echo "Building for Windows (amd64)..."
	GOOS=windows GOARCH=amd64 go build -o $(APP_NAME)-windows-amd64.exe ./cmd/agent

# Clean up built binaries
clean:
	@echo "Cleaning up..."
	rm -f $(APP_NAME) $(APP_NAME).exe $(APP_NAME)-linux-amd64 $(APP_NAME)-windows-amd64.exe

# Build and immediately run the application
run: build
	@echo "Running $(APP_NAME)..."
	./$(APP_NAME)