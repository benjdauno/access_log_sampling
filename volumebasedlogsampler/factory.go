// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package volumebasedlogsampler // import "github.com/open-telemetry/opentelemetry-collector-contrib/processor/attributesprocessor"

import (
	"context"
	"fmt"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
)

var (
	typeStr = component.MustNewType("volumebasedlogsampler")
)

const (
	defaultSamplingRate      = 1.0
	defaultExcludedEndpoints = "/etc/exclusions.txt"
	defaultPrometheusURL     = "http://localhost:9090"
	defaultLogLevel          = "info"
	environment              = "dev"
	refreshInterval          = "3m"
)

func createDefaultConfig() component.Config {
	return &Config{
		DefaultSamplingRate:         defaultSamplingRate,
		ExcludedEndpointsConfigFile: defaultExcludedEndpoints,
		PrometheusURL:               defaultPrometheusURL,
		LogLevel:                    defaultLogLevel,
		Environment:                 environment,
		RefreshInterval:             refreshInterval,
	}
}

func WithLogs(createLogsProcessor processor.CreateLogsFunc, sl component.StabilityLevel) processor.FactoryOption {
	return nil
}

func createLogsProcessor(_ context.Context, params processor.Settings, baseCfg component.Config, consumer consumer.Logs) (processor.Logs, error) {
	logger := params.Logger
	logSamplerCfg := baseCfg.(*Config)
	if err := logSamplerCfg.Validate(); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}
	logProcessor := &volumeBasedLogSamplerProcessor{
		logger:       logger,
		nextConsumer: consumer,
		config:       logSamplerCfg,
	}

	return logProcessor, nil
}

func NewFactory() processor.Factory {
	return processor.NewFactory(
		typeStr,
		createDefaultConfig,
		processor.WithLogs(createLogsProcessor, component.StabilityLevelDevelopment))
}
