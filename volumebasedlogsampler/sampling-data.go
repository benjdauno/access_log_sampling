package volumebasedlogsampler

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"go.uber.org/zap"
)

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

// getQueryInterval calculates the query interval based on the environment.
func (volBLogProc *volumeBasedLogSamplerProcessor) getQueryInterval() time.Duration {
	switch volBLogProc.config.Environment {
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
		// build the URL from the prometheus URL
		apiURL := fmt.Sprintf("%s/api/v1/query", volBLogProc.config.PrometheusURL)

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

	// Read exclusions from the config file
	exclusions, err := readExclusions(volBLogProc.config.ExcludedEndpointsConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		volBLogProc.logger.Warn("Exclusions file not found, sampling will apply to all endpoints")
	} else {
		if err != nil {
			volBLogProc.logger.Error("Failed to read exclusions", zap.Error(err))
		}
	}
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
	// Add exclusions with a sampling rate of 1.0
	for key := range exclusions {
		newRates[key] = 1.0
	}
	return newRates
}

// executePrometheusQuery sends a Prometheus query and parses the response.
func executePrometheusQuery(apiURL, query, token string) ([]map[string]interface{}, error) {
	client := &http.Client{Timeout: 120 * time.Second}
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

// readExclusions reads the list of excluded endpoints from the specified text file.
func readExclusions(filePath string) (map[string]bool, error) {
	exclusions := make(map[string]bool)
	file, err := os.Open(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		exclusions[line] = true
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return exclusions, nil
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
