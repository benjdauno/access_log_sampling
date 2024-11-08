package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"text/tabwriter"
)

// Define a list of endpoints to exclude from sampling
var excludedEndpoints = map[string]bool{
	"/important_endpoint": true,
	// Add more endpoints as needed
}

func main() {
	// Set the Prometheus API URL
	apiURL := "https://affirm.chronosphere.io/data/metrics/api/v1/query"

	// Define queries with regex to exclude health check paths in the HTTP query
	httpQuery := `sum(sum_over_time(http_server_handled_total{environment="prod",mode="live",path!~"/ping|/healthz|/_healthz"}[30d])) by (path)`
	rpcQuery := `sum(sum_over_time(rpc2_server_handled_total{environment="prod",mode="live"}[30d])) by (rpc2_method)`

	// Perform both queries and display their results
	fmt.Println("HTTP Path Metrics (excluding health check endpoints):")
	queryAndPrintResults(apiURL, httpQuery, "path")

	fmt.Println("\nRPC Method Metrics:")
	queryAndPrintResults(apiURL, rpcQuery, "rpc2_method")
}

// queryAndPrintResults executes a Prometheus query and prints results in a table with sampling rates
func queryAndPrintResults(apiURL, query, metricLabel string) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		log.Fatalf("Failed to create HTTP request: %v", err)
	}

	// Set the query as a URL parameter
	q := req.URL.Query()
	q.Add("query", query)
	req.URL.RawQuery = q.Encode()

	// Add headers with your API token
	req.Header.Add("Authorization", "Bearer TOKEN")
	req.Header.Add("Accept", "application/json")

	// Execute the request
	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("Failed to make request: %v", err)
	}
	defer resp.Body.Close()

	// Check for successful status code
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := ioutil.ReadAll(resp.Body)
		log.Fatalf("Failed to get data: %s", bodyBytes)
	}

	// Read and parse the response body
	var result map[string]interface{}
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read response body: %v", err)
	}

	err = json.Unmarshal(bodyBytes, &result)
	if err != nil {
		log.Fatalf("Failed to parse JSON: %v", err)
	}

	// Extract and print the data in a table format
	data, ok := result["data"].(map[string]interface{})
	if !ok {
		log.Fatalf("Unexpected data format")
	}

	results, ok := data["result"].([]interface{})
	if !ok {
		log.Fatalf("Unexpected result format")
	}

	// Prepare the table writer
	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.Debug)
	fmt.Fprintf(writer, "%s\tTotal Volume\tSampling Rate\n", metricLabel)
	fmt.Fprintf(writer, "-----------\t------------\t-------------\n")

	// Iterate over the results and print each entry in a table format
	for _, item := range results {
		entry := item.(map[string]interface{})
		metric, metricOk := entry["metric"].(map[string]interface{})
		if !metricOk {
			continue
		}

		// Retrieve label (path or rpc2_method) dynamically based on provided metricLabel
		label, labelOk := metric[metricLabel].(string)
		if !labelOk {
			label = "unknown"
		}

		// Extract the total volume from the value field
		value, valueOk := entry["value"].([]interface{})
		if !valueOk || len(value) < 2 {
			continue
		}
		totalVolumeStr := value[1].(string)
		totalVolume, err := strconv.ParseFloat(totalVolumeStr, 64)
		if err != nil {
			log.Printf("Failed to parse total volume for %s: %v", label, err)
			continue
		}

		// Calculate sampling rate based on volume guidelines
		samplingRate := calculateSamplingRate(totalVolume)

		// Apply exclusion if endpoint is in excludedEndpoints list
		if excludedEndpoints[label] {
			samplingRate = "Do Not Sample"
		}

		fmt.Fprintf(writer, "%s\t%.2f\t%s\n", label, totalVolume, samplingRate)
	}

	// Flush the writer to output the table
	writer.Flush()
}

// calculateSamplingRate determines the sampling rate based on monthly volume
func calculateSamplingRate(volume float64) string {
	switch {
	case volume > 10_000_000:
		return "1%"
	case volume > 1_000_000:
		return "2%"
	case volume > 500_000:
		return "5%"
	case volume > 200_000:
		return "10%"
	case volume > 50_000:
		return "20%"
	case volume < 50_000:
		return "Do Not Sample"
	default:
		return "Do Not Sample"
	}
}
