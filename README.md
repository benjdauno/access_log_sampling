# Affirm Custom Opentelemetry Collector


## What is this?

The Opentelemetry (OTEL) community has provided the means to extend the opentelemetry collector (otelcol) so that anyone can modify it and build custom components (receivers, processors, exporters and connectors).

This repo is Affirm's codebase for custom otelcol components.

## Context

There's a lot of baggage required to work with this area of technology. First, familiarity with the [OpenTelemetry](https://opentelemetry.io/docs/) project is a must. Reading up on the project in general, and more specifically, the [collector](https://opentelemetry.io/docs/collector/), will help users and contributors understand this codebase.

Next, this repository is specifically based on the OTEL documentation to build [custom collectors](https://opentelemetry.io/docs/collector/custom-collector/) and [components](https://opentelemetry.io/docs/collector/building/). While the tutorial goes through building a trace receiver, most of the principles apply to any custom component, and going through the tutorial is the best way to acquire familiarity with the concepts used in this codebase.

## Laptop Setup / Prerequisites

- [The Opentelemetry Collector Builder](https://opentelemetry.io/docs/collector/custom-collector/) (ocb)
- Golang (Any verison >1.21 should be fine)
- [telemetrygen](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/cmd/telemetrygen) utility
- Docker & Docker Compose

### VolumeBasedLogSampler

This sampler was originally developped to sample Envoy Access Logs at rates inversely proportional to their volume. That is endpoints and services that get the most traffic will have the lowest sampling rates.
