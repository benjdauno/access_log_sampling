package volumebasedlogsampler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"net/http"
	"os"
	"strconv"
	"sync"
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
	mu            sync.RWMutex       // Mutex to handle concurrent map access
	environment   string             // "dev", "stage", or "prod"
	queryInterval time.Duration      // Query interval for development
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
	volBLogProc.mu.Lock()
	defer volBLogProc.mu.Unlock()

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
	// Log the processing of logs
	volBLogProc.logger.Info("Processing logs batch",
		zap.Int("resourceLogsCount", ld.ResourceLogs().Len()))

	// Iterate through each ResourceLogs item to log details of the logs processed
	for i := 0; i < ld.ResourceLogs().Len(); i++ {
		resourceLogs := ld.ResourceLogs().At(i)
		for j := 0; j < resourceLogs.ScopeLogs().Len(); j++ {
			scopeLogs := resourceLogs.ScopeLogs().At(j)
			for k := 0; k < scopeLogs.LogRecords().Len(); k++ {
				logRecord := scopeLogs.LogRecords().At(k)
				endpointValue, exists := logRecord.Attributes().Get("endpoint")
				if !exists {
					volBLogProc.logger.Warn("Endpoint attribute missing in log record")
					continue
				}

				volBLogProc.logger.Info("Processing log record",
					zap.String("endpoint", endpointValue.Str()),
					zap.Time("timestamp", logRecord.Timestamp().AsTime()),
				)
			}
		}
	}

	// Forward all logs to the next consumer
	return volBLogProc.nextConsumer.ConsumeLogs(ctx, ld)
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
	volBLogProc.mu.Lock()
	defer volBLogProc.mu.Unlock()
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
		label := item["label"].(string)
		totalVolume, _ := strconv.ParseInt(item["value"].(string), 10, 64)
		newRates[label] = calculateSamplingRate(totalVolume)
	}

	return newRates
}

// executePrometheusQuery sends a Prometheus query and parses the response.
func executePrometheusQuery(apiURL, query, token string) ([]map[string]interface{}, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	q := req.URL.Query()
	q.Add("query", query)
	req.URL.RawQuery = q.Encode()

	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get data: %s", bodyBytes)
	}

	var result map[string]interface{}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(bodyBytes, &result)

	data, _ := result["data"].(map[string]interface{})
	rawResults, _ := data["result"].([]map[string]interface{})
	return rawResults, nil
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
