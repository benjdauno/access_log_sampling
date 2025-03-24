# Start from a Go image for convenience
FROM golang:1.23-bullseye

# Install prerequisites
RUN apt-get update && apt-get install -y \
    ca-certificates \
    curl \
    gnupg \
    lsb-release \
    kafkacat \
    siege \
    && rm -rf /var/lib/apt/lists/*

# Install the Docker CLI
RUN curl -fsSL https://get.docker.com -o get-docker.sh \
    && sh get-docker.sh \
    && rm get-docker.sh

# Install a stable version of Docker Compose 
RUN curl -L "https://github.com/docker/compose/releases/download/v2.20.2/docker-compose-linux-x86_64" -o /usr/local/bin/docker-compose \
    && chmod +x /usr/local/bin/docker-compose

# Install the OpenTelemetry Collector Builder (ocb) & Go debugger
RUN curl --proto '=https' --tlsv1.2 -fL -o /tmp/ocb \
    https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/cmd%2Fbuilder%2Fv0.119.0/ocb_0.119.0_linux_amd64 && \
    chmod +x /tmp/ocb && \
    mv /tmp/ocb /usr/local/bin/ocb

RUN go install github.com/go-delve/delve/cmd/dlv@latest &&\
    go install github.com/open-telemetry/opentelemetry-collector-contrib/cmd/telemetrygen@v0.119.0 &&\
    go install golang.org/x/tools/cmd/goimports@v0.30.0 &&\
    go install golang.org/x/tools/gopls@v0.16.1



WORKDIR /workspace
