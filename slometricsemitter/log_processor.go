package slometricsemitter

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"os"
	"time"

	"gopkg.in/yaml.v3"

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
	sloConfig            SLOConfig // Contains latency SLO definitions
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

	sloMetricsProc.logger.Info("Starting log processor with config", zap.String("sloConfigFile", sloMetricsProc.config.SLOConfigFile))
	sloMetricsProc.queryInterval = 2 * time.Minute

	// Pull SLO config, at this point it should be a valid yaml file
	var err error
	sloMetricsProc.sloConfig, err = sloMetricsProc.readSLOConfigFile(sloMetricsProc.config.SLOConfigFile)
	if err != nil {
		sloMetricsProc.logger.Error("Failed to read slo yaml file", zap.Error(err))
		return err
	}

	// Set up internal otel telemetry
	meter := sloMetricsProc.meterProvider.Meter("slo_metrics_processor")

	// Initialize custom counters. Labels are added at increment time.
	sloMetricsProc.latencyBreachCounter, err = meter.Int64Counter(
		"aff_otel_slo_latency_breaches_total",
		metric.WithDescription("Number of latency SLO breaches per endpoint and status code"),
		metric.WithUnit("1"),
	)
	if err != nil {
		sloMetricsProc.logger.Error("Failed to create latency breach counter", zap.Error(err))
		return err
	}

	sloMetricsProc.requestCounter, err = meter.Int64Counter(
		"aff_otel_requests_total",
		metric.WithDescription("Number of requests per endpoint and status code"),
		metric.WithUnit("1"),
	)
	if err != nil {
		sloMetricsProc.logger.Error("Failed to create request counter", zap.Error(err))
		return err
	}

	// TODO: Set up periodic fetching of SLO definitions
	// go sloMetricsProc.refreshSLODefinitions(ctx, token)
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
				err := sloMetricsProc.processLog(ctx, logRecord)
				if err != nil {
					sloMetricsProc.logger.Warn("Failed to process latency breach", zap.Error(err))
				}
			}
		}
	}

	return sloMetricsProc.nextConsumer.ConsumeLogs(ctx, ld)
}

func (sloMetricsProc *sloMetricsProcessor) processLog(ctx context.Context, logRecord plog.LogRecord) error {
	endpointType, err := sloMetricsProc.determineEndpointType(logRecord)
	if err != nil {
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
		return err
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
		return err
	}

	// Increment request counter
	sloMetricsProc.requestCounter.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("endpoint", endpoint),
			attribute.String("endpoint_type", endpointType),
			attribute.Int64("status_code", statusCode),
			attribute.String("method", method),
		),
	)

	objective, err := sloMetricsProc.getEndpointSLO(endpoint, method, endpointType)
	if err != nil {
		// No SLO found for endpoint, just debug log for now and skip processing
		sloMetricsProc.logger.Debug("SLO not found for endpoint",
			zap.String("endpoint", endpoint),
			zap.String("method", method),
			zap.String("endpoint_type", endpointType),
		)
		return nil
	}

	// Check for latency breaches, and increment breach counter for each quantile breached
	for quantile, threshold := range objective.Latency {
		if duration > threshold.Seconds() {
			sloMetricsProc.latencyBreachCounter.Add(ctx, 1,
				metric.WithAttributes(
					attribute.String("endpoint", endpoint),
					attribute.Int64("status_code", statusCode),
					attribute.String("endpoint_type", endpointType),
					attribute.String("method", method),
					attribute.Float64("objective", threshold.Seconds()),
					attribute.String("quantile", quantile),
				),
			)
		}
	}

	sloMetricsProc.logger.Debug("Processed log",
		zap.String("endpoint", endpoint),
		zap.String("endpointType", endpointType),
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

type EndpointSLOConfig struct {
	Latency     map[string]time.Duration `yaml:"latency"`
	SuccessRate float64                  `yaml:"success_rate"`
}

type SLOConfig struct {
	SLOs map[string]map[string]EndpointSLOConfig `yaml:"slos"`
}

// Custom unmarshal for EndpointSLOConfig to convert duration strings to time.Duration.
func (c *EndpointSLOConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// temporary struct to hold raw values
	var raw struct {
		Latency     map[string]string `yaml:"latency"`
		SuccessRate float64           `yaml:"success_rate"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}

	c.Latency = make(map[string]time.Duration)
	for quantile, durStr := range raw.Latency {
		d, err := time.ParseDuration(durStr)
		if err != nil {
			return fmt.Errorf("failed to parse duration %q for quantile %s: %w", durStr, quantile, err)
		}
		c.Latency[quantile] = d
	}
	c.SuccessRate = raw.SuccessRate
	return nil
}

func (sloMetricsProc *sloMetricsProcessor) readSLOConfigFile(sloConfigFile string) (SLOConfig, error) {
	var sloConfig SLOConfig

	f, err := os.ReadFile(sloConfigFile)
	if err != nil {
		return SLOConfig{}, fmt.Errorf("failed to read slo_config_file at %q: %w", sloConfigFile, err)
	}

	// Uses custom UnmarshalYAML for EndpointSLOConfig
	if err := yaml.Unmarshal(f, &sloConfig); err != nil {
		return SLOConfig{}, err
	}
	sloMetricsProc.logger.Info("Read SLO config file", zap.Any("sloConfig", sloConfig))

	return sloConfig, nil
}

func (sloMetricsProc *sloMetricsProcessor) getEndpointSLO(endpoint string, method string, endpointType string) (EndpointSLOConfig, error) {
	var key string
	switch {
	case endpointType == "rpc2" || endpointType == "grpc":
		key = endpoint
	case endpointType == "http":
		key = method + " " + endpoint
	default:
		return EndpointSLOConfig{}, fmt.Errorf("unknown endpoint type: %s", endpointType)
	}

	slo, exists := sloMetricsProc.sloConfig.SLOs[endpointType][key]
	if !exists {
		return EndpointSLOConfig{}, fmt.Errorf("SLO not found for endpoint: %s and method: %s", key, method)
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
