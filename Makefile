# Variables
COLLECTOR_NAME := affirm-otelcol
DOCKER_IMAGE := affirm-otelcol:0.0.7
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
	cd $(BUILD_DIR) && CGO_ENABLED=0 $(GO) build -o ../bin

# Build the Docker image
image:
	@echo "Building the Docker image..."
	$(DOCKER) build -t $(DOCKER_IMAGE) -f $(DOCKERFILE) .

# Run telemetrygen to generate log traffic
test:
	@echo "Generating log traffic with telemetrygen..."
	$(TELEMETRYGEN) logs --duration 30s --workers 4 --otlp-insecure --telemetry-attributes content_type=\"http\" \
	--telemetry-attributes x_affirm_endpoint_name=\"some/dummy/path\"

# Read messages from Kafka topic
read-kafka:
	@echo "Reading messages from Kafka topic from the beginning..."
	kafkacat -b localhost:29092 -t otel-logs -C -o beginning

read-kafka-current:
	@echo "Reading messages from Kafka topic from the current offset..."
	kafkacat -b localhost:29092 -t otel-logs -C

# Clean up build artifacts
clean:
	@echo "Cleaning up build artifacts..."
	rm -rf $(BUILD_DIR)/$(COLLECTOR_NAME) bin
