package volumebasedlogsampler

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"

	"os"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// volumeBasedLogSamplerProcessor is a custom processor that fetches endpoint volumes from Prometheus.
type volumeBasedLogSamplerProcessor struct {
	host          component.Host
	cancel        context.CancelFunc
	logger        *zap.Logger
	nextConsumer  consumer.Logs
	config        *Config
	samplingRates map[string]float32 // To store sampling rates based on endpoint volume

	environment   string        // "dev", "stage", or "prod"
	queryInterval time.Duration // Query interval for development
}

// Capabilities implements processor.Logs.
func (volBLogProc *volumeBasedLogSamplerProcessor) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: true}
}

// Start fetches data from Prometheus and stores sampling rates in a map.
// Overwriting ctx produces a warning, in the vscode go linter, but this is the recommendation from OTEL, so until otherwise indicated by OTEL docs,
// Please leave the ctx manipulation as is.
func (volBLogProc *volumeBasedLogSamplerProcessor) Start(ctx context.Context, host component.Host) error {
	volBLogProc.host = host
	ctx = context.Background()
	ctx, volBLogProc.cancel = context.WithCancel(ctx)
	// Set the log level from the environment variable
	volBLogProc.setLogLevel()
	// Initialize the map to store sampling rates
	volBLogProc.samplingRates = make(map[string]float32)
	volBLogProc.environment = "dev"
	volBLogProc.queryInterval = 2 * time.Minute
	// Fetch the token from the environment
	token := os.Getenv("PROMETHEUS_API_TOKEN")
	if token == "" {
		volBLogProc.logger.Error("PROMETHEUS_API_TOKEN environment variable not set")
		return fmt.Errorf("PROMETHEUS_API_TOKEN not set")
	}
	if err := volBLogProc.getSamplingRates(token); err != nil {
		return fmt.Errorf("failed to fetch initial sampling rates: %w", err)
	}
	// Set up periodic query execution
	go volBLogProc.refreshSamplingRates(ctx, token)
	return nil
}

// ConsumeLogs implements processor.Logs and will use the samplingRates map in future logic.
//func (volBLogProc *volumeBasedLogSamplerProcessor) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
//	// Add your ConsumeLogs processing here, where you could access volBLogProc.samplingRates map
//	return volBLogProc.nextConsumer.ConsumeLogs(ctx, ld)
//}

func (volBLogProc *volumeBasedLogSamplerProcessor) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	sampledLD := plog.NewLogs()

	volBLogProc.logger.Info("Processing logs batch",
		zap.Int("resourceLogsCount", ld.ResourceLogs().Len()))

	for i := 0; i < ld.ResourceLogs().Len(); i++ {
		resourceLogs := ld.ResourceLogs().At(i)
		newResourceLogs := sampledLD.ResourceLogs().AppendEmpty()
		resourceLogs.Resource().CopyTo(newResourceLogs.Resource())

		for j := 0; j < resourceLogs.ScopeLogs().Len(); j++ {
			scopeLogs := resourceLogs.ScopeLogs().At(j)
			newScopeLogs := newResourceLogs.ScopeLogs().AppendEmpty()
			scopeLogs.Scope().CopyTo(newScopeLogs.Scope())

			for k := 0; k < scopeLogs.LogRecords().Len(); k++ {
				logRecord := scopeLogs.LogRecords().At(k)

				endpointType, err := volBLogProc.determineEndpointType(logRecord)
				if err != nil {
					volBLogProc.logger.Warn("Failed to determine endpoint type", zap.Error(err))
					continue
				}

				endpoint, err := volBLogProc.extractEndpoint(logRecord, endpointType)
				if err != nil {
					volBLogProc.logger.Warn("Failed to extract endpoint", zap.Error(err))
					continue
				}

				if volBLogProc.shouldSample(endpoint) {
					newLogRecord := newScopeLogs.LogRecords().AppendEmpty()
					logRecord.CopyTo(newLogRecord)
					volBLogProc.logger.Debug("Log record retained",
						zap.String("endpoint", endpoint),
						zap.String("endpoint_type", endpointType),
						zap.Time("timestamp", logRecord.Timestamp().AsTime()))
				} else {
					volBLogProc.logger.Debug("Log record dropped due to sampling",
						zap.String("endpoint", endpoint),
						zap.String("endpoint_type", endpointType))
				}
			}
		}
	}

	return volBLogProc.nextConsumer.ConsumeLogs(ctx, sampledLD)
}

func (volBLogProc *volumeBasedLogSamplerProcessor) determineEndpointType(logRecord plog.LogRecord) (string, error) {
	contentTypeVal, exists := logRecord.Attributes().Get("content_type")
	if !exists {
		return "", fmt.Errorf("content_type attribute missing")
	}
	contentType := contentTypeVal.Str()

	switch {
	case strings.HasPrefix(contentType, "application/rpc2"):
		return "rpc2", nil
	case strings.HasPrefix(contentType, "application/grpc"):
		return "grpc", nil
	default:
		return "http", nil
	}
}

func (volBLogProc *volumeBasedLogSamplerProcessor) extractEndpoint(logRecord plog.LogRecord, endpointType string) (string, error) {
	switch endpointType {
	case "rpc2", "grpc":
		pathVal, exists := logRecord.Attributes().Get("path")
		if !exists || pathVal.Str() == "" {
			return "", fmt.Errorf("path attribute missing or empty for RPC2/GRPC endpoint")
		}
		pathSegments := strings.Split(strings.TrimPrefix(pathVal.Str(), "/"), "/")
		if len(pathSegments) < 2 {
			return "", fmt.Errorf("unable to parse endpoint from path")
		}
		// Reconstruct /service/method format
		return "/" + pathSegments[0] + "/" + pathSegments[1], nil
	case "http":
		endpointNameVal, exists := logRecord.Attributes().Get("x_affirm_endpoint_name")
		if !exists || endpointNameVal.Str() == "" || endpointNameVal.Str() == "-" {
			return "", fmt.Errorf("x_affirm_endpoint_name attribute missing or invalid for HTTP endpoint")
		}
		return endpointNameVal.Str(), nil
	default:
		return "", fmt.Errorf("unknown endpoint type: %s", endpointType)
	}
}

func (volBLogProc *volumeBasedLogSamplerProcessor) shouldSample(endpoint string) bool {
	rate, exists := volBLogProc.samplingRates[endpoint]
	if !exists {
		rate = 1.0 // default to keep if no rate is set
	}
	return rand.Float32() <= rate
}

// Shutdown gracefully shuts down the processor.
func (volBLogProc *volumeBasedLogSamplerProcessor) Shutdown(ctx context.Context) error {
	if volBLogProc.cancel != nil {
		volBLogProc.cancel()
	}
	return nil
}

// setLogLevel sets the log level based on the environment variable LOG_LEVEL.
func (volBLogProc *volumeBasedLogSamplerProcessor) setLogLevel() {
	logLevel := os.Getenv("LOG_LEVEL")
	var level zapcore.Level
	switch logLevel {
	case "debug":
		level = zapcore.DebugLevel
	case "info":
		level = zapcore.InfoLevel
	case "warn":
		level = zapcore.WarnLevel
	case "error":
		level = zapcore.ErrorLevel
	default:
		level = zapcore.InfoLevel
	}

	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(level)
	logger, err := config.Build()
	if err != nil {
		panic(fmt.Sprintf("Failed to create logger: %v", err))
	}
	volBLogProc.logger = logger
}
