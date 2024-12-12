## Laptop Setup / Prerequisites

- DevContainers extension (VSCode preferred)
- Docker & Docker Compose\
- Prometheus (Chronosphere) API Token

This project uses a devcontainer as a development environment as a means to provide a homogenous development experience to anyone working on the project. Contributors will need to build the development container before starting, and can then open it from the menu on the bottom left corner of VSCode.

The devcontainer installs:

- Golang version 1.23 
- [The Opentelemetry Collector Builder](https://opentelemetry.io/docs/collector/custom-collector/) (ocb) version 0.115.0
- dlv (Golang debugger)
- Docker CLI & Docker Compose(Note that the docker socket is actually shared with the host machine, but at the time or writing, this is fine.)
- telemetrygen (for generating logs to send)

## Manual steps

Create a .env file at the root of this project, with an entry for your PROMETHEUS_API_TOKEN

```
PROMETHEUS_API_TOKEN=XXXXXXXXXXXXX
```

### Starting the devcontainer

1. Ensure the devcontainer extension is installed in VSCode
2. Click on the bottom left corner of the VSCode window and select "Reopen in container"

The first time this is done, VSCode will build the devcontainer image for you, and then open a window in the development environment.

Subsequent sessions will be quicker.

### Running the binary

1. Build the binary with `make binary`
2. `export PROMETHEUS_API_TOKEN=XXXXXXXXXXXXX`
3. `./affirm-otelcol/affirm-otelcol --config volumebasedlogsampler/config.yaml`

