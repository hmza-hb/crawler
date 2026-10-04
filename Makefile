.PHONY: all build test test-coverage lint clean docker-build docker-up docker-down

APP_NAME := crawld
BUILD_DIR := bin
DOCKER_IMAGE := hmza-hb/crawld:latest

all: lint test build

build:
	@mkdir -p $(BUILD_DIR)
	go build -ldflags="-w -s" -o $(BUILD_DIR)/$(APP_NAME) ./cmd/crawld

test:
	go test -v -race ./...

test-coverage:
	go test -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -html=coverage.out -o coverage.html

lint:
	@which golangci-lint > /dev/null || (echo "golangci-lint not installed" && exit 0)
	golangci-lint run ./...

clean:
	rm -rf $(BUILD_DIR) coverage.out coverage.html

docker-build:
	docker build -t $(DOCKER_IMAGE) .

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down -v
