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

// sloMetricsProcessor is a custom processor reads SLO definitions from a yaml file
// and processes logs to increment counters for latency breaches and requests.
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
	environment          string // "dev", "stage", or "prod"
}

type IstioAccessLog struct {
	EndpointType string
	Endpoint     string
	StatusCode   int64
	Method       string
	Duration     float64
}

func (sloMetricsProc *sloMetricsProcessor) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

// Overwriting ctx produces a warning, in the vscode go linter, but this is the recommendation from OTEL, so until otherwise indicated by OTEL docs,
// Please leave the ctx manipulation as is.
func (sloMetricsProc *sloMetricsProcessor) Start(ctx context.Context, host component.Host) error {
	sloMetricsProc.host = host
	ctx = context.Background()
	ctx, sloMetricsProc.cancel = context.WithCancel(ctx)
	sloMetricsProc.setLogLevel()

	sloMetricsProc.logger.Info("Starting log processor with config and log level",
		zap.String("sloConfigFile", sloMetricsProc.config.SLOConfigFile),
		zap.String("logLevel", sloMetricsProc.config.LogLevel),
	)

	// Pull SLO config, at this point it should be a valid yaml file
	var err error
	sloMetricsProc.sloConfig, err = sloMetricsProc.readSLOConfigFile(sloMetricsProc.config.SLOConfigFile)
	if err != nil {
		sloMetricsProc.logger.Error("Failed to read slo yaml file", zap.Error(err))
		return err
	}

	sloMetricsProc.setupTelemetry()

	// TODO: Set up periodic fetching of SLO definitions
	// go sloMetricsProc.refreshSLODefinitions(ctx, token)
	return nil
}

func (sloMetricsProc *sloMetricsProcessor) setupTelemetry() error {
	var err error

	// Uses otel's metric sdk to expose metrics on the shared telemetry service
	meter := sloMetricsProc.meterProvider.Meter("slo_metrics_processor")
	// Initialize custom affirm SLO counters. For this sdk, labels are set at observation time.
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

	return nil
}

func (sloMetricsProc *sloMetricsProcessor) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	sloMetricsProc.logger.Debug("Processing logs batch",
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
					sloMetricsProc.logger.Debug("Log record",
						zap.Any("attributes", logRecord.Attributes().AsRaw()),
					)
				}
			}
		}
	}

	// Pass on unmodified logs to the next consumer
	return sloMetricsProc.nextConsumer.ConsumeLogs(ctx, ld)
}

func (sloMetricsProc *sloMetricsProcessor) processLog(ctx context.Context, logRecord plog.LogRecord) error {
	accessLog, err := sloMetricsProc.extractIstioAccessLogFromLogRecord(logRecord)
	if err != nil {
		return err
	}

	sloMetricsProc.requestCounter.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("endpoint", accessLog.Endpoint),
			attribute.String("endpoint_type", accessLog.EndpointType),
			attribute.Int64("status_code", accessLog.StatusCode),
			attribute.String("method", accessLog.Method),
		),
	)

	objective, err := sloMetricsProc.getEndpointSLO(accessLog.Endpoint, accessLog.Method, accessLog.EndpointType)
	if err != nil {
		// No SLO found for endpoint, just debug log for now and skip processing
		sloMetricsProc.logger.Debug("SLO not found for endpoint",
			zap.String("endpoint", accessLog.Endpoint),
			zap.String("method", accessLog.Method),
			zap.String("endpoint_type", accessLog.EndpointType),
		)
		return nil
	}

	// Check for latency breaches, and increment breach counter for each quantile breached
	for quantile, threshold := range objective.Latency {
		if accessLog.Duration > threshold.Seconds() {
			sloMetricsProc.latencyBreachCounter.Add(ctx, 1,
				metric.WithAttributes(
					attribute.String("endpoint", accessLog.Endpoint),
					attribute.Int64("status_code", accessLog.StatusCode),
					attribute.String("endpoint_type", accessLog.EndpointType),
					attribute.String("method", accessLog.Method),
					attribute.Float64("objective", threshold.Seconds()),
					attribute.String("quantile", quantile),
				),
			)
		}
	}

	sloMetricsProc.logger.Debug("Processed log",
		zap.String("endpoint", accessLog.Endpoint),
		zap.String("endpointType", accessLog.EndpointType),
		zap.Int64("status_code", accessLog.StatusCode),
		zap.String("method", accessLog.Method),
		zap.Float64("duration", accessLog.Duration),
	)

	return nil
}

func (sloMetricsProc *sloMetricsProcessor) extractIstioAccessLogFromLogRecord(logRecord plog.LogRecord) (IstioAccessLog, error) {
	endpointType := sloMetricsProc.determineEndpointType(logRecord)

	endpoint, err := sloMetricsProc.extractEndpoint(logRecord, endpointType)
	if err != nil {
		return IstioAccessLog{}, err
	}

	statusCodeVal, err := sloMetricsProc.getAttributeFromLogRecord(logRecord, "response_code")
	if err != nil {
		return IstioAccessLog{}, err
	}
	statusCode, err := strconv.ParseInt(statusCodeVal, 10, 64)
	if err != nil {
		return IstioAccessLog{}, err
	}

	method, err := sloMetricsProc.getAttributeFromLogRecord(logRecord, "method")
	if err != nil {
		return IstioAccessLog{}, err
	}

	durationAttr, err := sloMetricsProc.getAttributeFromLogRecord(logRecord, "duration")
	if err != nil {
		return IstioAccessLog{}, err
	}
	duration, err := strconv.ParseFloat(durationAttr, 64)
	if err != nil {
		return IstioAccessLog{}, err
	}

	return IstioAccessLog{
		EndpointType: endpointType,
		Endpoint:     endpoint,
		StatusCode:   statusCode,
		Method:       method,
		Duration:     duration,
	}, nil
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

// Defaults to http if content_type attribute is missing or unknown.
func (sloMetricsProc *sloMetricsProcessor) determineEndpointType(logRecord plog.LogRecord) string {
	contentTypeVal, exists := logRecord.Attributes().Get("content_type")
	if !exists {
		sloMetricsProc.logger.Debug("content_type attribute missing from log record", zap.Any("attributes", logRecord.Attributes().AsRaw()))
		return "http"
	}
	contentType := contentTypeVal.Str()

	switch {
	case strings.HasPrefix(contentType, "application/rpc2"):
		return "rpc2"
	case strings.HasPrefix(contentType, "application/grpc"):
		return "grpc"
	default:
		sloMetricsProc.logger.Debug("Unknown content type, defaulting to http",
			zap.String("content_type", contentType),
		)
		return "http"
	}
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

func (sloMetricsProc *sloMetricsProcessor) getAttributeFromLogRecord(logRecord plog.LogRecord, attribute string) (string, error) {
	attrVal, exists := logRecord.Attributes().Get(attribute)
	if !exists || attrVal.Str() == "" {
		sloMetricsProc.logger.Warn(attribute + " attribute missing or empty from log record")
		return "", fmt.Errorf(attribute + " attribute missing or empty from log record")
	}
	return attrVal.Str(), nil
}

type EndpointSLOConfig struct {
	Latency     map[string]time.Duration `yaml:"latency"`
	SuccessRate float64                  `yaml:"success_rate"`
}

type SLOConfig struct {
	SLOs map[string]map[string]EndpointSLOConfig `yaml:"slos"`
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

func (sloMetricsProc *sloMetricsProcessor) Shutdown(ctx context.Context) error {
	if sloMetricsProc.cancel != nil {
		sloMetricsProc.cancel()
	}
	return nil
}

// setLogLevel sets the log level. Takes env variable as precedent, then falls back to processor config.
func (sloMetricsProc *sloMetricsProcessor) setLogLevel() {

	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" && sloMetricsProc.config != nil {
		logLevel = sloMetricsProc.config.LogLevel
	}

	var level zapcore.Level
	switch strings.ToLower(logLevel) {
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
		sloMetricsProc.logger.Info("Invalid or empty log level, defaulting to INFO",
			zap.String("provided_level", logLevel))
	}

	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(level)
	logger, err := config.Build()
	if err != nil {
		panic(fmt.Sprintf("Failed to create logger: %v", err))
	}
	sloMetricsProc.logger = logger
}
