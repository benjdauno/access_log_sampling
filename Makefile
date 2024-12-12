# Variables
COLLECTOR_NAME := affirm-otelcol
DOCKER_IMAGE := affirm-otelcol:latest
DOCKERFILE := Dockerfile
BUILD_DIR := ./affirm-otelcol

# Commands
OCB := ocb
GO := go
DOCKER := docker
TELEMETRYGEN := /go/bin/telemetrygen

.PHONY: all build collector binary docker image clean run test

# Default target: Build the entire project
all: collector binary image

# Build the OpenTelemetry Collector components using ocb
collector:
	@echo "Building OpenTelemetry Collector components..."
	$(OCB) --config /workspaces/access_log_sampling/builder-config.yaml --output-path=$(BUILD_DIR) &&\
	mkdir -p bin && mv ${BUILD_DIR}/${COLLECTOR_NAME} bin/

# Build the binary using Go
binary:
	@echo "Building the binary..."
	cd $(BUILD_DIR) && $(GO) build -o ../bin

# Run telemetrygen to generate log traffic
test:
	@echo "Generating log traffic with telemetrygen..."
	$(TELEMETRYGEN) logs --duration 10s --workers 4 --otlp-insecure

# Build the Docker image
image:
	@echo "Building the Docker image..."
	$(DOCKER) build -t $(DOCKER_IMAGE) -f $(DOCKERFILE) .

# Clean up build artifacts
clean:
	@echo "Cleaning up build artifacts..."
	rm -rf $(BUILD_DIR)/$(COLLECTOR_NAME) bin
