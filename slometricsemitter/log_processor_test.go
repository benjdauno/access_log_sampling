package slometricsemitter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/plog"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.uber.org/zap/zaptest"
)

// Helper function to create a processor with a temporary SLO config file
func createProcessorWithConfig(t *testing.T, configYaml string) (*sloMetricsProcessor, sdkmetric.Reader) {
	logger := zaptest.NewLogger(t)
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	// Create a temporary SLO config file
	tmpDir := t.TempDir()
	sloFilePath := filepath.Join(tmpDir, "slo.yaml")
	require.NoError(t, os.WriteFile(sloFilePath, []byte(configYaml), 0600))

	proc := &sloMetricsProcessor{
		logger:        logger,
		config:        &Config{SLOConfigFile: sloFilePath},
		meterProvider: meterProvider,
		nextConsumer:  consumertest.NewNop(),
	}

	return proc, reader
}

// TestSLOMetricsProcessorStartShutdown verifies that the processor can start and shut down
// without error. It also checks that reading an SLO config file works when the file exists
// (and fails when the file does not exist).
func TestSLOMetricsProcessorStartShutdown(t *testing.T) {
	configYaml := `
slos:
  http:
    "GET /test/path":
      latency:
        "0.5": "0.1s"
        "0.99": "0.9s"
      success_rate: 99.5
`
	proc, _ := createProcessorWithConfig(t, configYaml)

	// Start should succeed
	err := proc.Start(context.Background(), componenttest.NewNopHost())
	require.NoError(t, err, "expected no error on Start")

	// Check that the SLO config was read
	require.NotNil(t, proc.sloConfig.SLOs["http"])
	endpointCfg, exists := proc.sloConfig.SLOs["http"]["GET /test/path"]
	require.True(t, exists)
	assert.InDelta(t, 0.1, endpointCfg.Latency["0.5"].Seconds(), 0.001)
	assert.InDelta(t, 0.9, endpointCfg.Latency["0.99"].Seconds(), 0.001)
	assert.Equal(t, 99.5, endpointCfg.SuccessRate)

	require.NoError(t, proc.Shutdown(context.Background()))
}

// TestSLOMetricsProcessorStartInvalidFile ensures an error occurs if the SLO config file is missing
func TestSLOMetricsProcessorStartInvalidFile(t *testing.T) {
	logger := zaptest.NewLogger(t)
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	proc := &sloMetricsProcessor{
		logger:        logger,
		config:        &Config{SLOConfigFile: "/non/existent/file.yaml"},
		meterProvider: meterProvider,
		nextConsumer:  consumertest.NewNop(),
	}

	err := proc.Start(context.Background(), componenttest.NewNopHost())
	require.Error(t, err)
}

// TestSLOMetricsProcessorConsumeLogs tests that the processor increments the appropriate counters
// and forwards logs to the next consumer.
func TestSLOMetricsProcessorConsumeLogs(t *testing.T) {
	configYaml := `
slos:
  http:
    "GET /test/path":
      latency:
        "0.5": "0.1s"
        "0.99": "0.5s"
      success_rate: 99.0
  grpc:
    "/service/method":
      latency:
        "0.95": "0.2s"
      success_rate: 99.9
`

	tests := []struct {
		name             string
		attributes       map[string]string
		expectedBreaches int
		shouldError      bool
	}{
		{
			name: "HTTP request within SLO",
			attributes: map[string]string{
				"method":                 "GET",
				"response_code":          "200",
				"content_type":           "application/json",
				"x_affirm_endpoint_name": "/test/path",
				"duration":               "0.050",
			},
			expectedBreaches: 0,
		},
		{
			name: "HTTP request breaching 0.5 SLO",
			attributes: map[string]string{
				"method":                 "GET",
				"response_code":          "200",
				"content_type":           "application/json",
				"x_affirm_endpoint_name": "/test/path",
				"duration":               "0.300",
			},
			expectedBreaches: 1,
		},
		{
			name: "HTTP request breaching both SLOs",
			attributes: map[string]string{
				"method":                 "GET",
				"response_code":          "200",
				"content_type":           "application/json",
				"x_affirm_endpoint_name": "/test/path",
				"duration":               "0.600",
			},
			expectedBreaches: 2,
		},
		{
			name: "gRPC request breaching SLO",
			attributes: map[string]string{
				"method":        "POST",
				"response_code": "200",
				"content_type":  "application/grpc",
				"path":          "/service/method",
				"duration":      "0.300",
			},
			expectedBreaches: 1,
		},
		{
			name: "Unknown endpoint",
			attributes: map[string]string{
				"method":                 "GET",
				"response_code":          "200",
				"content_type":           "application/json",
				"x_affirm_endpoint_name": "/unknown/path",
				"duration":               "0.300",
			},
			expectedBreaches: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			proc, reader := createProcessorWithConfig(t, configYaml)
			logSink := new(consumertest.LogsSink)
			proc.nextConsumer = logSink

			require.NoError(t, proc.Start(context.Background(), componenttest.NewNopHost()))

			ld := plog.NewLogs()
			lr := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()

			for k, v := range tc.attributes {
				lr.Attributes().PutStr(k, v)
			}

			err := proc.ConsumeLogs(context.Background(), ld)
			if tc.shouldError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, logSink.AllLogs(), 1)

			var metrics metricdata.ResourceMetrics
			err = reader.Collect(context.Background(), &metrics)
			require.NoError(t, err)

			breachCount, requestCount := countMetrics(metrics)

			assert.Equal(t, tc.expectedBreaches, breachCount, "unexpected number of breaches")
			assert.GreaterOrEqual(t, requestCount, 1, "should have at least one request count")

			require.NoError(t, proc.Shutdown(context.Background()))
		})
	}
}

// TestSLOMetricsProcessorProcessLog tests internal method processLog
func TestSLOMetricsProcessorProcessLog(t *testing.T) {
	configYaml := `
slos:
  http:
    "POST /my/endpoint":
      latency:
        "0.5": "0.5s"
        "0.99": "1.5s"
`
	proc, reader := createProcessorWithConfig(t, configYaml)

	require.NoError(t, proc.Start(context.Background(), componenttest.NewNopHost()))

	tests := []struct {
		name              string
		duration          float64
		expectBreachCount int
	}{
		{"below_0.5_threshold", 0.2, 0},
		{"between_0.5_and_0.99_threshold", 0.7, 1},
		{"above_0.99_threshold", 2.0, 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lr := plog.NewLogRecord()
			lr.Attributes().PutStr("method", "POST")
			lr.Attributes().PutStr("response_code", "200")
			lr.Attributes().PutStr("content_type", "application/json")
			lr.Attributes().PutStr("x_affirm_endpoint_name", "/my/endpoint")
			lr.Attributes().PutStr("duration", floatToStr(tc.duration))

			err := proc.processLog(context.Background(), lr)
			require.NoError(t, err)

			var metrics metricdata.ResourceMetrics
			err = reader.Collect(context.Background(), &metrics)
			require.NoError(t, err)

			breachCount, _ := countMetrics(metrics)
			assert.Equal(t, tc.expectBreachCount, breachCount, "unexpected number of breaches")
		})
	}

	require.NoError(t, proc.Shutdown(context.Background()))
}

func floatToStr(f float64) string {
	return fmt.Sprintf("%.3f", f)
}

// TestSLOMetricsProcessorMissingAttributes ensures we get an error (and a warning) if required attributes are missing.
func TestSLOMetricsProcessorMissingAttributes(t *testing.T) {
	logger := zaptest.NewLogger(t)
	proc := &sloMetricsProcessor{logger: logger}
	lr := plog.NewLogRecord()

	err := proc.processLog(context.Background(), lr)
	require.Error(t, err)
}

func TestEndpointTypeDetection(t *testing.T) {
	tests := []struct {
		name         string
		contentType  string
		expectedType string
	}{
		{
			name:         "HTTP content type",
			contentType:  "application/json",
			expectedType: "http",
		},
		{
			name:         "HTTP content type with charset",
			contentType:  "application/json; charset=utf-8",
			expectedType: "http",
		},
		{
			name:         "GRPC content type",
			contentType:  "application/grpc",
			expectedType: "grpc",
		},
		{
			name:         "RPC2 content type",
			contentType:  "application/rpc2",
			expectedType: "rpc2",
		},
		{
			name:         "Missing content type",
			expectedType: "http",
		},
		{
			name:         "Unknown content type",
			contentType:  "unknown/content-type",
			expectedType: "http",
		},
	}

	proc := &sloMetricsProcessor{logger: zaptest.NewLogger(t)}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lr := plog.NewLogRecord()
			if tc.contentType != "" {
				lr.Attributes().PutStr("content_type", tc.contentType)
			}

			endpointType := proc.determineEndpointType(lr)
			assert.Equal(t, tc.expectedType, endpointType)
		})
	}
}

func TestEndpointExtraction(t *testing.T) {
	tests := []struct {
		name         string
		endpointType string
		attributes   map[string]string
		expectedPath string
		shouldError  bool
	}{
		{
			name:         "Valid HTTP endpoint",
			endpointType: "http",
			attributes: map[string]string{
				"x_affirm_endpoint_name": "/api/v1/users",
			},
			expectedPath: "/api/v1/users",
		},
		{
			name:         "Valid gRPC endpoint",
			endpointType: "grpc",
			attributes: map[string]string{
				"path": "/service/method",
			},
			expectedPath: "/service/method",
		},
		{
			name:         "Invalid gRPC path format",
			endpointType: "grpc",
			attributes: map[string]string{
				"path": "/invalid",
			},
			shouldError: true,
		},
		{
			name:         "Missing HTTP endpoint name",
			endpointType: "http",
			attributes: map[string]string{
				"x_affirm_endpoint_name": "-",
			},
			shouldError: true,
		},
		{
			name:         "Unknown endpoint type",
			endpointType: "unexpected",
			shouldError:  true,
		},
	}

	proc := &sloMetricsProcessor{logger: zaptest.NewLogger(t)}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lr := plog.NewLogRecord()
			for k, v := range tc.attributes {
				lr.Attributes().PutStr(k, v)
			}

			endpoint, err := proc.extractEndpoint(lr, tc.endpointType)
			if tc.shouldError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.expectedPath, endpoint)
		})
	}
}

func TestSLOConfigParsing(t *testing.T) {
	tests := []struct {
		name        string
		configYaml  string
		shouldError bool
	}{
		{
			name: "Valid config with multiple services",
			configYaml: `
slos:
  http:
    "GET /api/v1":
      latency:
        "0.95": "0.1s"
      success_rate: 99.9
  grpc:
    "/service/method":
      latency:
        "0.99": "0.5s"
      success_rate: 99.5
`,
		},
		{
			name: "Invalid duration format",
			configYaml: `
slos:
  http:
    "GET /api/v1":
      latency:
        "0.95": "invalid"
      success_rate: 99.9
`,
			shouldError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpFile := filepath.Join(t.TempDir(), "slo.yaml")
			require.NoError(t, os.WriteFile(tmpFile, []byte(tc.configYaml), 0600))

			proc := &sloMetricsProcessor{logger: zaptest.NewLogger(t)}

			_, err := proc.readSLOConfigFile(tmpFile)
			if tc.shouldError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// Helper function to count breaches and requests from metrics
func countMetrics(metrics metricdata.ResourceMetrics) (breachCount, requestCount int) {
	for _, m := range metrics.ScopeMetrics {
		for _, instrument := range m.Metrics {
			if instrument.Name == "aff_otel_slo_latency_breaches_total" {
				if sum, ok := instrument.Data.(metricdata.Sum[int64]); ok {
					breachCount += len(sum.DataPoints)
				}
			} else if instrument.Name == "aff_otel_requests_total" {
				if sum, ok := instrument.Data.(metricdata.Sum[int64]); ok {
					requestCount += len(sum.DataPoints)
				}
			}
		}
	}
	return
}
