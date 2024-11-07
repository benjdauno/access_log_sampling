// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package volumebasedlogsampler // import "github.com/open-telemetry/opentelemetry-collector-contrib/processor/attributesprocessor"

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
)

var (
	typeStr = component.MustNewType("volumebasedlogsampler")
)

const (
	defaultSamplingRate = 1.0
)

// Note: This isn't a valid configuration because the processor would do no work.
func createDefaultConfig() component.Config {
	return &Config{
		SamplingRate:      defaultSamplingRate,
		ExcludedEndpoints: []string{},
	}
}

//func WithLogs(createLogsProcessor processor.CreateLogsFunc, sl component.StabilityLevel) processor.FactoryOption

func createLogsProcessor(_ context.Context, params processor.Settings, baseCfg component.Config, consumer consumer.Logs) (processor.Logs, error) {
	logger := params.Logger
	logSamplerCfg := baseCfg.(*Config)
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
