// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package slometricsemitter // import "github.com/open-telemetry/opentelemetry-collector-contrib/processor/attributesprocessor"

import (
	"context"
	"fmt"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
)

var (
	typeStr = component.MustNewType("slometricsemitter")
)

const (
	defaultLogLevel = "info"
	environment     = "dev"
)

func createDefaultConfig() component.Config {
	return &Config{
		LogLevel:    defaultLogLevel,
		Environment: environment,
	}
}

func WithLogs(createLogsProcessor processor.CreateLogsFunc, sl component.StabilityLevel) processor.FactoryOption {
	return nil
}

func createLogsProcessor(_ context.Context, params processor.Settings, baseCfg component.Config, consumer consumer.Logs) (processor.Logs, error) {
	logger := params.Logger
	sloMetricsEmitterConfig := baseCfg.(*Config)
	if err := sloMetricsEmitterConfig.Validate(); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}
	logProcessor := &sloMetricsProcessor{
		logger:        logger,
		meterProvider: params.MeterProvider,
		nextConsumer:  consumer,
		config:        sloMetricsEmitterConfig,
	}

	return logProcessor, nil
}

func NewFactory() processor.Factory {
	return processor.NewFactory(
		typeStr,
		createDefaultConfig,
		processor.WithLogs(createLogsProcessor, component.StabilityLevelDevelopment))
}
