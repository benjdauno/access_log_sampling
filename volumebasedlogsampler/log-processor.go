package volumebasedlogsampler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"

	"net/http"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
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
					volBLogProc.logger.Debug("Log record sampled",
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

// refreshSamplingRates periodically fetches and updates sampling rates based on the environment.
func (volBLogProc *volumeBasedLogSamplerProcessor) refreshSamplingRates(ctx context.Context, token string) {
	ticker := time.NewTicker(volBLogProc.getQueryInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			volBLogProc.logger.Info("Running queries to refresh sampling rates")
			if err := volBLogProc.getSamplingRates(token); err != nil {
				volBLogProc.logger.Error("Failed to run scheduled queries", zap.Error(err))
			}
		}
	}
}

// getQueryInterval calculates the query interval based on the environment.
func (volBLogProc *volumeBasedLogSamplerProcessor) getQueryInterval() time.Duration {
	switch volBLogProc.environment {
	case "dev":
		return volBLogProc.queryInterval // Short interval for debugging in development.
	case "stage", "prod":
		now := time.Now()
		nextMonth := now.AddDate(0, 1, 0)
		firstDayNextMonth := time.Date(nextMonth.Year(), nextMonth.Month(), 1, 0, 0, 0, 0, time.UTC)
		return time.Until(firstDayNextMonth) // Schedule monthly.
	default:
		return time.Hour * 24 // Default to daily queries if environment is unknown.
	}
}

// getSamplingRates fetches Prometheus data and updates the sampling rates atomically.
func (volBLogProc *volumeBasedLogSamplerProcessor) getSamplingRates(token string) error {
	// Fetch data from Prometheus.
	rawData, err := volBLogProc.fetchPrometheusData(token)
	if err != nil {
		return err
	}

	// Build new sampling rate table.
	newSamplingRates := volBLogProc.buildSamplingRateTable(rawData)

	// Atomically update the map.
	volBLogProc.samplingRates = newSamplingRates

	return nil
}

// fetchPrometheusData executes Prometheus queries and returns raw data.
func (volBLogProc *volumeBasedLogSamplerProcessor) fetchPrometheusData(token string) ([]map[string]interface{}, error) {
	queries := []struct {
		baseQuery   string
		metricLabel string
	}{
		{
			baseQuery:   `sum(sum_over_time(http_server_handled_total{environment="prod",mode="live",path!~"/ping|/healthz|/_healthz"}[%s])) by (path)`,
			metricLabel: "path",
		},
		{
			baseQuery:   `sum(sum_over_time(rpc2_server_handled_total{environment="prod",mode="live"}[%s])) by (rpc2_method)`,
			metricLabel: "rpc2_method",
		},
	}

	offset := calculatePreviousMonthOffset()
	var results []map[string]interface{}

	for _, q := range queries {
		query := fmt.Sprintf(q.baseQuery, offset)
		apiURL := "https://affirm.chronosphere.io/data/metrics/api/v1/query"

		resp, err := executePrometheusQuery(apiURL, query, token)
		if err != nil {
			volBLogProc.logger.Error("Failed to query Prometheus", zap.String("query", query), zap.Error(err))
			return nil, err
		}
		results = append(results, resp...)
	}

	return results, nil
}

// buildSamplingRateTable builds a new sampling rates map from Prometheus data.
func (volBLogProc *volumeBasedLogSamplerProcessor) buildSamplingRateTable(data []map[string]interface{}) map[string]float32 {
	newRates := make(map[string]float32)

	for _, item := range data {
		// Extract the label map
		metric := item["metric"].(map[string]interface{})

		// Extract the label key and value (assumes there's only one key-value pair in the map)
		var labelValue string

		for _, value := range metric {

			labelValue = value.(string)
			break // Only one label key is expected
		}

		// Extract volume
		volumeStr := item["value"].([]interface{})[1].(string)
		totalVolume, _ := strconv.ParseInt(volumeStr, 10, 64)

		// Store the sampling rate using the label value as the key
		newRates[labelValue] = calculateSamplingRate(totalVolume)
	}

	return newRates
}

// executePrometheusQuery sends a Prometheus query and parses the response.
func executePrometheusQuery(apiURL, query, token string) ([]map[string]interface{}, error) {
	client := &http.Client{Timeout: 90 * time.Second}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Add query parameters
	q := req.URL.Query()
	q.Add("query", query)
	req.URL.RawQuery = q.Encode()

	// Set headers
	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Accept", "application/json")

	// Execute the HTTP request
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get data: %s", bodyBytes)
	}

	// Parse the response body
	var parsedResponse struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string                   `json:"resultType"`
			Result     []map[string]interface{} `json:"result"`
		} `json:"data"`
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if err := json.Unmarshal(bodyBytes, &parsedResponse); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	// Check the status field
	if parsedResponse.Status != "success" {
		return nil, fmt.Errorf("Prometheus query failed with status: %s", parsedResponse.Status)
	}

	return parsedResponse.Data.Result, nil
}

// calculatePreviousMonthOffset calculates the Prometheus offset for the previous month.
func calculatePreviousMonthOffset() string {
	now := time.Now()
	firstDayOfCurrentMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	firstDayOfPreviousMonth := firstDayOfCurrentMonth.AddDate(0, -1, 0)
	duration := time.Since(firstDayOfPreviousMonth)
	return fmt.Sprintf("%dh", int(duration.Hours()))
}

// calculateSamplingRate calculates the sampling rate based on monthly volume.
func calculateSamplingRate(volume int64) float32 {
	switch {
	case volume > 10_000_000:
		return 0.01
	case volume > 1_000_000:
		return 0.02
	case volume > 500_000:
		return 0.05
	case volume > 200_000:
		return 0.1
	case volume > 50_000:
		return 0.2
	default:
		return 1.0
	}
}
