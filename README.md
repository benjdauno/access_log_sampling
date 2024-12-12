# Affirm Custom Opentelemetry Collector


## What is this?

The Opentelemetry (OTEL) community has provided the means to extend the opentelemetry collector (otelcol) so that anyone can modify it and build custom components (receivers, processors, exporters and connectors).

This repo is Affirm's codebase for custom otelcol components.

## Context

There's a lot of baggage required to work with this area of technology. Familiarity with the [OpenTelemetry](https://opentelemetry.io/docs/) project is a must. Reading up on the project in general, and more specifically, the [collector](https://opentelemetry.io/docs/collector/), will help users and contributors understand this codebase.

Next, this repository is specifically based on the OTEL documentation to build [custom collectors](https://opentelemetry.io/docs/collector/custom-collector/) and [components](https://opentelemetry.io/docs/collector/building/). While the tutorial goes through building a trace receiver, most of the principles apply to any custom component (receiver, processor, or exporter), and going through the tutorial is the best way to acquire familiarity with the concepts used in this codebase. Connectors are another class of custom component not currently in scope.

## Custom Components

### Volume based log sampler

This component was specifically developed to sample a percentage of the Envoy access logs that Affirm uses to calculate service latencies (with p99 being the most relevant to our SLAs). Since we have so many data points coming in, we can sample only some of them, and still calculate p99 accurately. Endpoints with the highest volume will have the lowest sampling rates.

This component:

1.  Gets a list of services and request volumes from a Prometheus data source
2.  Sets a sampling rate for each service based on the volume of requests that it receives.
3. Processes incoming access logs and applies a sampling rate based on the log attributes, or a default sampling rate. 

