# Variables
COLLECTOR_NAME := affirm-otelcol
DOCKER_IMAGE := affirm-otelcol:0.0.16-alpha-1
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
	$(OCB) --config /workspaces/access_log_sampling/builder-config.yaml &&\
	mkdir -p bin && mv ${BUILD_DIR}/${COLLECTOR_NAME} bin/

# Build the binary using Go
binary:
	@echo "Building the binary..."
	cd $(BUILD_DIR) && CGO_ENABLED=0 $(GO) build -o ../bin

# Build the Docker image
image:
	@echo "Building the Docker image..."
	$(DOCKER) build -t $(DOCKER_IMAGE) -f $(DOCKERFILE) .

run:
	@echo "Running binary..."
	./bin/$(COLLECTOR_NAME) --config ./dev_tooling/volume-sampler-otel-config.yaml

run_slo:
	@echo "Running binary..."
	./bin/$(COLLECTOR_NAME) --config ./dev_tooling/slo-metrics-otel-config.yaml

# Run unit tests
test:
	@echo "Running unit tests..."
	$(GO) test ./slometricsemitter/...

# Run unit tests with coverage
test-coverage:
	@echo "Running unit tests with coverage..."
	$(GO) test -coverprofile=coverage.out ./slometricsemitter/...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated at coverage.html"

# Generate log traffic for testing
gen-logs:
	@echo "Generating log traffic with telemetrygen..."
	$(TELEMETRYGEN) logs --duration 30s --workers 4 --otlp-insecure --telemetry-attributes content_type=\"http\" \
	--telemetry-attributes x_affirm_endpoint_name=\"some/dummy/path\"

# Generate SLO-specific log traffic for testing
gen-slo-logs:
	@echo "Generating SLO-specific log traffic with telemetrygen..."
	$(TELEMETRYGEN) logs --logs 1 --otlp-insecure --telemetry-attributes content_type=\"http\" \
	--telemetry-attributes x_affirm_endpoint_name=\"/api/pf/authentication/v1/\" --telemetry-attributes status_code=\"200\" \
	--telemetry-attributes method=\"POST\" --telemetry-attributes duration=\"123\"
	
	$(TELEMETRYGEN) logs --logs 1 --otlp-insecure --telemetry-attributes content_type=\"http\" \
	--telemetry-attributes x_affirm_endpoint_name=\"/non-existant\" --telemetry-attributes status_code=\"200\" \
	--telemetry-attributes method=\"POST\" --telemetry-attributes duration=\"123\"
	
	$(TELEMETRYGEN) logs --logs 1 --otlp-insecure --telemetry-attributes content_type=\"application/rpc2\" \
	--telemetry-attributes path=\"/affirm.members.service.apis.api_v1/get_user_locale_v1\" \
	--telemetry-attributes status_code=\"200\" --telemetry-attributes method=\"POST\" \
	--telemetry-attributes duration=\"456\"

# Read messages from Kafka topic
read-kafka:
	@echo "Reading messages from Kafka topic from the beginning..."
	kafkacat -b localhost:29092 -t otel-logs -C -o beginning

read-kafka-current:
	@echo "Reading messages from Kafka topic from the current offset..."
	kafkacat -b localhost:29092 -t otel-logs -C

.PHONY: load-test
test-envoy:
# change this to use siege to test the envoy proxy
	@echo "Running load test..."
	siege -c 10 -t 30s -v -f /workspaces/access_log_sampling/dev_tooling/urls.txt 

# Clean up build artifacts
clean:
	@echo "Cleaning up build artifacts..."
	rm -rf $(BUILD_DIR)/$(COLLECTOR_NAME) bin
