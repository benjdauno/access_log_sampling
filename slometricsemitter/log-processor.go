package slometricsemitter

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"os"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// sloMetricsProcessor is a custom processor that fetches endpoint volumes from Prometheus.
type sloMetricsProcessor struct {
	host                 component.Host
	cancel               context.CancelFunc
	logger               *zap.Logger
	meterProvider        metric.MeterProvider
	nextConsumer         consumer.Logs
	config               *Config
	sloConfig            SloConfig // Contains latency SLO definitions
	latencyBreachCounter metric.Int64Counter
	requestCounter       metric.Int64Counter

	environment   string        // "dev", "stage", or "prod"
	queryInterval time.Duration // Query interval for development
}

// Capabilities implements processor.Logs.
func (sloMetricsProc *sloMetricsProcessor) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: true}
}

// Start fetches data from Prometheus and stores sampling rates in a map.
// Overwriting ctx produces a warning, in the vscode go linter, but this is the recommendation from OTEL, so until otherwise indicated by OTEL docs,
// Please leave the ctx manipulation as is.
func (sloMetricsProc *sloMetricsProcessor) Start(ctx context.Context, host component.Host) error {
	sloMetricsProc.host = host
	ctx = context.Background()
	ctx, sloMetricsProc.cancel = context.WithCancel(ctx)
	// Set the log level from the environment variable
	sloMetricsProc.setLogLevel()
	sloMetricsProc.environment = "dev"
	sloMetricsProc.queryInterval = 2 * time.Minute

	// Pull SLO config
	sloMetricsProc.sloConfig = sloMetricsProc.getSloConfig()

	// Set up internal otel telemetry
	meter := sloMetricsProc.meterProvider.Meter("slo_metrics_processor")
	var err error
	sloMetricsProc.latencyBreachCounter, err = meter.Int64Counter(
		"aff_otel_slo_latency_breach",
		metric.WithDescription("Number of latency SLO breaches per endpoint and status code"),
		metric.WithUnit("1"),
	)
	if err != nil {
		sloMetricsProc.logger.Error("Failed to create latency breach counter", zap.Error(err))
		return err
	}

	sloMetricsProc.requestCounter, err = meter.Int64Counter(
		"aff_otel_request",
		metric.WithDescription("Number of requests per endpoint and status code"),
		metric.WithUnit("1"),
	)
	if err != nil {
		sloMetricsProc.logger.Error("Failed to create request counter", zap.Error(err))
		return err
	}

	// TODO: Set up periodic fetching of SLO definitions
	// go sloMetricsProc.refreshSloDefinitions(ctx, token)
	return nil
}

func (sloMetricsProc *sloMetricsProcessor) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	sloMetricsProc.logger.Info("Processing logs batch",
		zap.Int("resourceLogsCount", ld.ResourceLogs().Len()))

	for i := 0; i < ld.ResourceLogs().Len(); i++ {
		resourceLogs := ld.ResourceLogs().At(i)
		for j := 0; j < resourceLogs.ScopeLogs().Len(); j++ {
			scopeLogs := resourceLogs.ScopeLogs().At(j)
			for k := 0; k < scopeLogs.LogRecords().Len(); k++ {
				logRecord := scopeLogs.LogRecords().At(k)
				err := sloMetricsProc.processLatencyBreach(ctx, logRecord)
				if err != nil {
					sloMetricsProc.logger.Warn("Failed to process latency breach", zap.Error(err))
				}
			}
		}
	}

	return sloMetricsProc.nextConsumer.ConsumeLogs(ctx, ld)
}

type EndpointSloConfig struct {
	latency     map[string]float64
	successRate float64
}

type SloConfig struct {
	endpoints map[string]EndpointSloConfig
}

func (sloMetricsProc *sloMetricsProcessor) getSloConfig() SloConfig {
	// TODO: Read from config yaml
	return SloConfig{
		endpoints: map[string]EndpointSloConfig{
			"POST /some/dummy/path": {
				latency: map[string]float64{
					"0.5":  0.5,
					"0.99": 0.75,
				},
				successRate: 0.999,
			},
		},
	}
}

func (sloMetricsProc *sloMetricsProcessor) processLatencyBreach(ctx context.Context, logRecord plog.LogRecord) error {
	endpointType, err := sloMetricsProc.determineEndpointType(logRecord)
	if err != nil {
		sloMetricsProc.logger.Warn("Failed to determine endpoint type", zap.Error(err))
		return err
	}

	endpoint, err := sloMetricsProc.extractEndpoint(logRecord, endpointType)
	if err != nil {
		return err
	}

	statusCodeVal, err := sloMetricsProc.getAttributeFromLogRecord(logRecord, "status_code")
	if err != nil {
		return err
	}
	statusCode, err := strconv.ParseInt(statusCodeVal, 10, 64)
	if err != nil {
		sloMetricsProc.logger.Warn("Failed to parse status_code as int64",
			zap.Any("status_code", statusCode),
			zap.Error(err),
		)
		return fmt.Errorf("failed to parse status_code: %v", err)
	}

	method, err := sloMetricsProc.getAttributeFromLogRecord(logRecord, "method")
	if err != nil {
		return err
	}

	durationAttr, err := sloMetricsProc.getAttributeFromLogRecord(logRecord, "duration")
	if err != nil {
		return err
	}
	duration, err := strconv.ParseFloat(durationAttr, 64)
	if err != nil {
		sloMetricsProc.logger.Warn("Failed to parse duration as float64",
			zap.Any("duration", duration),
			zap.Error(err),
		)
		return fmt.Errorf("failed to parse duration: %v", err)
	}

	objective, err := sloMetricsProc.getEndpointSlo(endpoint, method)
	if err != nil {
		sloMetricsProc.logger.Error(err.Error())
		return err
	}

	// Increment request counter
	sloMetricsProc.requestCounter.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("endpoint", endpoint),
			attribute.Int64("status_code", statusCode),
			attribute.String("method", method),
		),
	)

	// Check for latency breaches, and increment breach counter for each quantile breached
	for quantile, threshold := range objective.latency {
		if duration > threshold {
			sloMetricsProc.logger.Warn("incrementing latency breach counter")
			sloMetricsProc.latencyBreachCounter.Add(ctx, 1,
				metric.WithAttributes(
					attribute.String("endpoint", endpoint),
					attribute.Int64("status_code", statusCode),
					attribute.String("method", method),
					attribute.Float64("objective", threshold),
					attribute.String("quantile", quantile),
				),
			)
		}
	}

	sloMetricsProc.logger.Info("Processed latency breach",
		zap.String("endpoint", endpoint),
		zap.Int64("status_code", statusCode),
		zap.String("method", method),
		zap.String("duration", durationAttr),
	)

	return nil
}

func (sloMetricsProc *sloMetricsProcessor) getAttributeFromLogRecord(logRecord plog.LogRecord, attribute string) (string, error) {
	attrVal, exists := logRecord.Attributes().Get(attribute)
	if !exists || attrVal.Str() == "" {
		sloMetricsProc.logger.Warn(attribute + " attribute missing or empty from log record")
		return "", fmt.Errorf(attribute + " attribute missing or empty from log record")
	}
	return attrVal.Str(), nil
}

func (sloMetricsProc *sloMetricsProcessor) determineEndpointType(logRecord plog.LogRecord) (string, error) {
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

func (sloMetricsProc *sloMetricsProcessor) getEndpointSlo(endpoint string, method string) (EndpointSloConfig, error) {
	key := method + " " + endpoint
	slo, exists := sloMetricsProc.sloConfig.endpoints[key]
	if !exists {
		return EndpointSloConfig{}, fmt.Errorf("SLO not found for endpoint: %s", key)
	}
	return slo, nil
}

func (sloMetricsProc *sloMetricsProcessor) extractEndpoint(logRecord plog.LogRecord, endpointType string) (string, error) {
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

// Shutdown gracefully shuts down the processor.
func (sloMetricsProc *sloMetricsProcessor) Shutdown(ctx context.Context) error {
	if sloMetricsProc.cancel != nil {
		sloMetricsProc.cancel()
	}
	return nil
}

// setLogLevel sets the log level based on the environment variable LOG_LEVEL.
func (sloMetricsProc *sloMetricsProcessor) setLogLevel() {
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
	sloMetricsProc.logger = logger
}
