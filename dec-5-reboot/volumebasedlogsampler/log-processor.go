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

// volumeBasedLogSamplerProcessor is a custom processor that fetches endpoint volumes from Prometheus
type volumeBasedLogSamplerProcessor struct {
	host          component.Host
	cancel        context.CancelFunc
	logger        *zap.Logger
	nextConsumer  consumer.Logs
	config        *Config
	samplingRates map[string]float32 // To store sampling rates based on endpoint volume
	mu            sync.RWMutex       // Mutex to handle concurrent map access
}

// Capabilities implements processor.Logs.
func (volBLogProc *volumeBasedLogSamplerProcessor) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: true}
}

// Start fetches data from Prometheus and stores sampling rates in a map.
// Overwriting ctx produces a warning, in the vscode go linter, but this is how the OTEL docs do this, so until otherwise indicated by OTEL docs,
// Please leave the ctx manipulation as is.
func (volBLogProc *volumeBasedLogSamplerProcessor) Start(ctx context.Context, host component.Host) error {
	volBLogProc.host = host
	ctx = context.Background()
	ctx, volBLogProc.cancel = context.WithCancel(ctx)
	volBLogProc.mu.Lock()
	defer volBLogProc.mu.Unlock()

	// Initialize the map to store sampling rates
	volBLogProc.samplingRates = make(map[string]float32)

	// Fetch the token from the environment
	token := os.Getenv("PROMETHEUS_API_TOKEN")
	if token == "" {
		volBLogProc.logger.Error("PROMETHEUS_API_TOKEN environment variable not set")
		return fmt.Errorf("PROMETHEUS_API_TOKEN not set")
	}

	// Define the queries
	queries := []struct {
		query       string
		metricLabel string
	}{
		{
			query:       `sum(sum_over_time(http_server_handled_total{environment="prod",mode="live",path!~"/ping|/healthz|/_healthz"}[30d])) by (path)`,
			metricLabel: "path",
		},
		{
			query:       `sum(sum_over_time(rpc2_server_handled_total{environment="prod",mode="live"}[30d])) by (rpc2_method)`,
			metricLabel: "rpc2_method",
		},
	}

	// Execute each query and merge results
	for _, q := range queries {
		if err := volBLogProc.queryAndStoreSamplingRates("https://affirm.chronosphere.io/data/metrics/api/v1/query", q.query, q.metricLabel, token); err != nil {
			volBLogProc.logger.Error("Failed to query Prometheus", zap.String("query", q.query), zap.Error(err))
			return err
		}
	}

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

// queryAndStoreSamplingRates queries Prometheus and stores the sampling rate for each endpoint in a map
func (volBLogProc *volumeBasedLogSamplerProcessor) queryAndStoreSamplingRates(apiURL, query, metricLabel, token string) error {
	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Set query as URL parameter
	q := req.URL.Query()
	q.Add("query", query)
	req.URL.RawQuery = q.Encode()

	// Add headers
	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to get data: %s", bodyBytes)
	}

	// Parse the response body
	var result map[string]interface{}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return fmt.Errorf("failed to parse JSON: %w", err)
	}

	data, ok := result["data"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("unexpected data format")
	}

	results, ok := data["result"].([]interface{})
	if !ok {
		return fmt.Errorf("unexpected result format")
	}

	// Populate the samplingRates map
	for _, item := range results {
		entry := item.(map[string]interface{})
		metric, metricOk := entry["metric"].(map[string]interface{})
		if !metricOk {
			continue
		}

		label, labelOk := metric[metricLabel].(string)
		if !labelOk {
			label = "unknown"
		}

		// Get the volume
		value, valueOk := entry["value"].([]interface{})
		if !valueOk || len(value) < 2 {
			continue
		}
		totalVolumeStr := value[1].(string)
		totalVolume, err := strconv.ParseInt(totalVolumeStr, 10, 64)
		if err != nil {
			volBLogProc.logger.Warn("Failed to parse total volume", zap.String("label", label), zap.Error(err))
			continue
		}

		// Calculate sampling rate
		samplingRate := calculateSamplingRate(totalVolume)

		// Store the sampling rate in the map
		volBLogProc.samplingRates[label] = samplingRate
	}
	return nil
}

// calculateSamplingRate calculates the sampling rate based on monthly volume
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
