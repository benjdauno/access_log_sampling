package slometricsemitter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
	sync.Mutex    // Add a mutex for thread-safe operations
	s3Client             S3Client
}

type IstioAccessLog struct {
	EndpointType string
	Endpoint     string
	StatusCode   int64
	Method       string
	Duration     float64
}

var (
    httpLocalPath = "/tmp/slo_configs/slo_master_http.yaml"
    rpc2LocalPath = "/tmp/slo_configs/slo_master_rpc.yaml"
)

func (sloMetricsProc *sloMetricsProcessor) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

// Add this interface
type S3Client interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// Modify the FetchSLOConfigFromS3 method to accept a client
func (sloMetricsProc *sloMetricsProcessor) FetchSLOConfigFromS3(ctx context.Context, bucketName, httpLocalPath string, rpc2LocalPath string) error {
	// Read the region from the environment variable REGION set in the pod
	region := os.Getenv("REGION")
	if region == "" {
		// If REGION is not set in the environment, fallback to default region
		region = "us-east-1"
	}

	// Use the provided client or create a new one
	var s3Client S3Client
	if sloMetricsProc.s3Client != nil {
		s3Client = sloMetricsProc.s3Client
	} else {
		awsConfig, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
		if err != nil {
			sloMetricsProc.logger.Error("Failed to load AWS config", zap.Error(err))
			return err
		}
		s3Client = s3.NewFromConfig(awsConfig)
	}

	var wg sync.WaitGroup
	errChan := make(chan error, 2) // Buffer size of 2 to prevent blocking

	// S3 bucket keys
	httpS3Key := "slos/slo_master_http.yaml"
	rpc2S3Key := "slos/slo_master_rpc.yaml"

	// Fetch HTTP SLO config in a separate goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := sloMetricsProc.fetchAndWriteS3FileToLocal(ctx, s3Client, bucketName, httpS3Key, httpLocalPath, sloMetricsProc.logger); err != nil {
			errChan <- fmt.Errorf("failed to fetch HTTP SLO config: %w", err)
		}
	}()

	// Fetch RPC2 SLO config in another goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := sloMetricsProc.fetchAndWriteS3FileToLocal(ctx, s3Client, bucketName, rpc2S3Key, rpc2LocalPath, sloMetricsProc.logger); err != nil {
			errChan <- fmt.Errorf("failed to fetch RPC2 SLO config: %w", err)
		}
	}()

	// Wait for both goroutines to finish
	wg.Wait()
	close(errChan) // Close channel to signal no more errors will be sent

	// Collect errors, if any
	var finalErr error
	for err := range errChan {
		finalErr = err 
	}

	return finalErr
}

// fetchAndWriteS3FileToLocal fetches an object from S3 and writes it to a local file.
func (sloMetricsProc *sloMetricsProcessor) fetchAndWriteS3FileToLocal(ctx context.Context, s3Client S3Client, bucketName, key, localPath string, logger *zap.Logger) error {
	output, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		sloMetricsProc.logger.Error("Failed to fetch file from S3", zap.String("key", key), zap.Error(err))
		return err
	}
	defer output.Body.Close()

	var buf bytes.Buffer
	_, err = io.Copy(&buf, output.Body)
	if err != nil {
		sloMetricsProc.logger.Error("Failed to read S3 object content", zap.String("key", key), zap.Error(err))
		return err
	}
	
	err = os.WriteFile(localPath, buf.Bytes(), 0644)
	if err != nil {
		sloMetricsProc.logger.Error("Failed to write SLO config file", zap.String("path", localPath), zap.Error(err))
		return err
	}


	// Reload and merge configurations
	err = sloMetricsProc.reloadAndMergeSLOConfig()
	if err != nil {
		sloMetricsProc.logger.Error("Failed to reload and merge SLO configurations", zap.Error(err))
		return err
	}
	return nil
}

// Overwriting ctx produces a warning, in the vscode go linter, but this is the recommendation from OTEL, so until otherwise indicated by OTEL docs,
// Please leave the ctx manipulation as is.
func (sloMetricsProc *sloMetricsProcessor) Start(ctx context.Context, host component.Host) error {
	sloMetricsProc.host = host
	ctx = context.Background()
	ctx, sloMetricsProc.cancel = context.WithCancel(ctx)
	sloMetricsProc.setLogLevel()

	// Fetch SLO config initially
	bucketName := os.Getenv("S3_BUCKET")
	if bucketName == "" {
		// Set a default bucket name if S3_BUCKET is not set in the environment
		bucketName = "affirm-prod-ops"
	}

	// Pull SLO config, at this point it should be a valid yaml file
	err := sloMetricsProc.FetchSLOConfigFromS3(ctx, bucketName, httpLocalPath, rpc2LocalPath)
	if err != nil {
		dir, err := os.Getwd()
		if err != nil {
			sloMetricsProc.logger.Error("Error occurred", zap.Error(err))
		}
		sloMetricsProc.logger.Error("Current Directory", zap.String("directory", dir))
		sloMetricsProc.logger.Error("Failed to fetch initial SLO config", zap.Error(err))
		return err
	}

	
	// Start periodic refresh every hour
	
	go sloMetricsProc.startPeriodicRefresh(ctx, bucketName, httpLocalPath, rpc2LocalPath, time.Hour)


	// Set up internal otel telemetry
	sloMetricsProc.setupTelemetry()


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


func (sloMetricsProc *sloMetricsProcessor) startPeriodicRefresh(ctx context.Context, bucketName, httpLocalPath, rpc2LocalPath string, interval time.Duration) {
	sloMetricsProc.logger.Info("Refreshing the slo config files")
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := sloMetricsProc.FetchSLOConfigFromS3(ctx, bucketName, httpLocalPath, rpc2LocalPath)
			if err != nil {
				sloMetricsProc.logger.Error("Failed to refresh SLO config from S3", zap.Error(err))
				continue
			}
		}
	}
}


func (sloMetricsProc *sloMetricsProcessor) reloadAndMergeSLOConfig() error {

	// Read HTTP SLO config
	httpConfig, err := sloMetricsProc.readSLOConfigFile(httpLocalPath)
	if err != nil {
		sloMetricsProc.logger.Error("Failed to reload HTTP SLO config file", zap.String("path", httpLocalPath), zap.Error(err))
		return err
	}

	// Read RPC2 SLO config
	rpc2Config, err := sloMetricsProc.readSLOConfigFile(rpc2LocalPath)
	if err != nil {
		sloMetricsProc.logger.Error("Failed to reload RPC2 SLO config file", zap.String("path", rpc2LocalPath), zap.Error(err))
		return err
	}

	// Merge both configurations safely
	sloMetricsProc.Lock()
	defer sloMetricsProc.Unlock()

	// If sloConfig is identical, avoid redundant updates
	if sloMetricsProc.hashConfig(sloMetricsProc.sloConfig) == sloMetricsProc.hashConfig(sloMetricsProc.mergeSLOConfigs(httpConfig, rpc2Config)) {
		sloMetricsProc.logger.Info("No changes detected, skipping config update.")
		return nil
	}

	sloMetricsProc.sloConfig = sloMetricsProc.mergeSLOConfigs(httpConfig, rpc2Config)

	return nil
}

func(sloMetricsProc *sloMetricsProcessor) hashConfig(config interface{}) string {
    b, _ := json.Marshal(config) // Serialize to JSON
    sum := sha256.Sum256(b)      // Compute SHA-256 hash
    return fmt.Sprintf("%x", sum)
}

func (sloMetricsProc *sloMetricsProcessor) mergeSLOConfigs(httpConfig, rpc2Config SLOConfig) SLOConfig {

    // Create a new SLOConfig to hold the merged result
    merged := SLOConfig{
		SLOs: make(map[string]map[string]EndpointSLOConfig), // Initialize the top-level map for SLOs
    }

    // Initialize the "http" section if it exists in the httpConfig
    if httpConfig.SLOs["http"] != nil {
		merged.SLOs["http"] = make(map[string]EndpointSLOConfig)
        for key, value := range httpConfig.SLOs["http"] {
            merged.SLOs["http"][key] = value
        }
    }

    // Initialize the "rpc2" section if it exists in the rpc2Config
    if rpc2Config.SLOs["rpc2"] != nil {
		merged.SLOs["rpc2"] = make(map[string]EndpointSLOConfig)
        for key, value := range rpc2Config.SLOs["rpc2"] {
            merged.SLOs["rpc2"][key] = value
        }
    }

	// Initialize the "grpc" section if it exists in the rpc2Config
    if rpc2Config.SLOs["grpc"] != nil {
        merged.SLOs["grpc"] = make(map[string]EndpointSLOConfig)
        for key, value := range rpc2Config.SLOs["grpc"] {
            merged.SLOs["grpc"][key] = value
        }
    }

    return merged
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

	sloMetricsProc.logger.Debug("Processing log",
		zap.String("endpoint", accessLog.Endpoint),
		zap.String("endpointType", accessLog.EndpointType),
		zap.Int64("status_code", accessLog.StatusCode),
		zap.String("method", accessLog.Method),
		zap.Float64("duration", accessLog.Duration),
	)

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
		sloMetricsProc.logger.Debug("SLO not found for endpoint", zap.Any("accessLog", accessLog))
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
		endpointType = "rpc2"
	case endpointType == "http":
		key = method + " " + endpoint
	default:
		return EndpointSLOConfig{}, fmt.Errorf("unknown endpoint type: %s", endpointType)
	}

	// Go doesn't panic when a non-existent endpointType key is attempted to be accessed
	slo, exists := sloMetricsProc.sloConfig.SLOs[endpointType][key]
	if !exists {
		return EndpointSLOConfig{}, fmt.Errorf("SLO not found for endpoint: %s and method: %s", key, method)
	}
	return slo, nil
}

// We don't strongly set content-type for our endpoints, so this generally defaults to http.
func (sloMetricsProc *sloMetricsProcessor) determineEndpointType(logRecord plog.LogRecord) string {
	contentType, err := sloMetricsProc.getAttributeFromLogRecord(logRecord, "content_type")
	if err != nil {
		return "http"
	}

	if strings.HasPrefix(contentType, "application/rpc2") {
		return "rpc2"
	} else if strings.HasPrefix(contentType, "application/grpc") {
		return "grpc"
	} else if strings.HasPrefix(contentType, "application/json") {
		return "http"
	} else {
		sloMetricsProc.logger.Debug("Could not determine endpoint type",
			zap.Any("attributes", logRecord.Attributes().AsRaw()),
		)
		return "http"
	}
}

func (sloMetricsProc *sloMetricsProcessor) extractEndpoint(logRecord plog.LogRecord, endpointType string) (string, error) {
	switch endpointType {
	case "rpc2", "grpc":
		path, err := sloMetricsProc.getAttributeFromLogRecord(logRecord, "path")
		if err != nil {
			return "", fmt.Errorf("path attribute missing or empty for RPC2/GRPC endpoint")
		}
		pathSegments := strings.Split(strings.TrimPrefix(path, "/"), "/")
		if len(pathSegments) < 2 {
			return "", fmt.Errorf("unable to parse endpoint from path")
		}
		// Reconstruct /service/method format
		return "/" + pathSegments[0] + "/" + pathSegments[1], nil
	case "http":
		endpointName, err := sloMetricsProc.getAttributeFromLogRecord(logRecord, "x_affirm_endpoint_name")
		if err != nil {
			return "", fmt.Errorf("x_affirm_endpoint_name attribute missing or empty for HTTP endpoint")
		}
		return endpointName, nil
	default:
		return "", fmt.Errorf("could not extract endpoint name with endpoint type: %s", endpointType)
	}
}

func (sloMetricsProc *sloMetricsProcessor) getAttributeFromLogRecord(logRecord plog.LogRecord, attribute string) (string, error) {
	attrVal, exists := logRecord.Attributes().Get(attribute)
	if !exists || attrVal.Str() == "" || attrVal.Str() == "-" {
		sloMetricsProc.logger.Debug(attribute+" attribute missing or empty from log record",
			zap.Any("attributes", logRecord.Attributes().AsRaw()),
		)
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

	// Ensure the config file exists before reading
	if err := ensureFileExists(sloConfigFile, sloMetricsProc.logger); err != nil {
		return SLOConfig{}, err
	}
	
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

// ensureFileExists checks if a file exists and creates an empty file if missing
func ensureFileExists(filePath string, logger *zap.Logger) error {
	// Extract the directory path
	dir := filepath.Dir(filePath)

	// Ensure the directory exists
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	// Check if the file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		logger.Info("SLO config file not found, creating an empty file", zap.String("file", filePath))

		// Create an empty file
		file, err := os.Create(filePath)
		if err != nil {
			return fmt.Errorf("failed to create empty SLO config file %s: %w", filePath, err)
		}
		file.Close() // Close the file after creating it
	}

	return nil
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
