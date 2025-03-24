package slometricsemitter

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/plog"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// Helper function to count metrics
func countMetrics(metrics metricdata.ResourceMetrics) (breachCount, requestCount int) {
	for _, scopeMetrics := range metrics.ScopeMetrics {
		for _, metric := range scopeMetrics.Metrics {
			switch metric.Name {
			case "aff_otel_slo_latency_breaches_total":
				if sum, ok := metric.Data.(metricdata.Sum[int64]); ok {
					for _, dp := range sum.DataPoints {
						breachCount += int(dp.Value)
					}
				}
			case "aff_otel_requests_total":
				if sum, ok := metric.Data.(metricdata.Sum[int64]); ok {
					for _, dp := range sum.DataPoints {
						requestCount += int(dp.Value)
					}
				}
			}
		}
	}
	return
}

// Helper function to verify metrics
func verifyMetrics(t *testing.T, reader sdkmetric.Reader, expectedBreaches, minRequests int) {
	var metrics metricdata.ResourceMetrics
	err := reader.Collect(context.Background(), &metrics)
	require.NoError(t, err)

	breachCount, requestCount := countMetrics(metrics)
	assert.Equal(t, expectedBreaches, breachCount, "unexpected number of breaches")
	assert.GreaterOrEqual(t, requestCount, minRequests, "unexpected number of requests")
}

func setupTestProcessor(t *testing.T) (*sloMetricsProcessor, sdkmetric.Reader) {
	logger := zap.NewNop()

	// Create a meter provider for testing
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	proc := &sloMetricsProcessor{
		logger:        logger,
		meterProvider: meterProvider,
		sloConfig: SLOConfig{
			SLOs: map[string]map[string]EndpointSLOConfig{
				"http": {
					"GET /test": {
						Latency: map[string]time.Duration{
							"p99": 1 * time.Second,
						},
						SuccessRate: 0.99,
					},
				},
				"rpc2": {
					"/service/method": {
						Latency: map[string]time.Duration{
							"p99": 500 * time.Millisecond,
						},
						SuccessRate: 0.99,
					},
				},
			},
		},
	}

	// Initialize the metrics
	err := proc.setupTelemetry()
	require.NoError(t, err)

	return proc, reader
}

func createTestLogRecord() plog.LogRecord {
	logs := plog.NewLogs()
	resourceLogs := logs.ResourceLogs().AppendEmpty()
	scopeLogs := resourceLogs.ScopeLogs().AppendEmpty()
	logRecord := scopeLogs.LogRecords().AppendEmpty()
	
	attrs := logRecord.Attributes()
	attrs.PutStr("response_code", "200")
	attrs.PutStr("method", "GET")
	attrs.PutStr("duration", "0.5")
	attrs.PutStr("x_affirm_endpoint_name", "/test")
	attrs.PutStr("content_type", "application/json")
	
	return logRecord
}

func TestExtractIstioAccessLogFromLogRecord(t *testing.T) {
	processor, reader := setupTestProcessor(t)
	logRecord := createTestLogRecord()

	accessLog, err := processor.extractIstioAccessLogFromLogRecord(logRecord)
	require.NoError(t, err)

	assert.Equal(t, "http", accessLog.EndpointType)
	assert.Equal(t, "/test", accessLog.Endpoint)
	assert.Equal(t, int64(200), accessLog.StatusCode)
	assert.Equal(t, "GET", accessLog.Method)
	assert.Equal(t, 0.5, accessLog.Duration)

	// Verify no metrics were generated during extraction
	verifyMetrics(t, reader, 0, 0)
}

// TestSLOMetricsProcessorMissingAttributes ensures we get an error (and a warning) if required attributes are missing.
func TestSLOMetricsProcessorMissingAttributes(t *testing.T) {
	logger := zaptest.NewLogger(t)
	proc := &sloMetricsProcessor{logger: logger}
	lr := plog.NewLogRecord()

	err := proc.processLog(context.Background(), lr)
	require.Error(t, err)
}

func TestDetermineEndpointType(t *testing.T) {
	processor, reader := setupTestProcessor(t)
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

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logRecord := createTestLogRecord()
			logRecord.Attributes().PutStr("content_type", tt.contentType)
			
			endpointType := processor.determineEndpointType(logRecord)
      assert.Equal(t, tt.expectedType, endpointType)
		})
	}

	// Verify no metrics were generated during type determination
	verifyMetrics(t, reader, 0, 0)
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

func TestGetEndpointSLO(t *testing.T) {
	processor, reader := setupTestProcessor(t)

	t.Run("HTTP endpoint SLO", func(t *testing.T) {
		slo, err := processor.getEndpointSLO("/test", "GET", "http")
		require.NoError(t, err)
		assert.Equal(t, time.Second, slo.Latency["p99"])
	})

	t.Run("RPC2 endpoint SLO", func(t *testing.T) {
		slo, err := processor.getEndpointSLO("/service/method", "", "rpc2")
		require.NoError(t, err)
		assert.Equal(t, 500*time.Millisecond, slo.Latency["p99"])
	})

	t.Run("Non-existent endpoint", func(t *testing.T) {
		_, err := processor.getEndpointSLO("/nonexistent", "GET", "http")
		assert.Error(t, err)
	})

	// Verify no metrics were generated during SLO retrieval
	verifyMetrics(t, reader, 0, 0)
}

func TestProcessLog(t *testing.T) {
	processor, reader := setupTestProcessor(t)
	ctx := context.Background()

	t.Run("Process valid log within SLO", func(t *testing.T) {
		logRecord := createTestLogRecord()
		err := processor.processLog(ctx, logRecord)
		assert.NoError(t, err)
		verifyMetrics(t, reader, 0, 1) // No breaches, 1 request
	})

	t.Run("Process log breaching SLO", func(t *testing.T) {
		logRecord := createTestLogRecord()
		logRecord.Attributes().PutStr("duration", "1.5") // Above p99 threshold
		err := processor.processLog(ctx, logRecord)
		assert.NoError(t, err)
		verifyMetrics(t, reader, 1, 2) // 1 breach, 2 total requests
	})

	t.Run("Process log with missing attributes", func(t *testing.T) {
		logRecord := plog.NewLogRecord()
		err := processor.processLog(ctx, logRecord)
		assert.Error(t, err)
		verifyMetrics(t, reader, 1, 2) // Metrics unchanged due to error
	})
}

func TestMetricsAccumulation(t *testing.T) {
	processor, reader := setupTestProcessor(t)
	ctx := context.Background()

	tests := []struct {
		name             string
		duration         string
		expectedBreaches int
		totalRequests    int
	}{
		{
			name:             "No breach",
			duration:         "0.5",
			expectedBreaches: 0,
			totalRequests:    1,
		},
		{
			name:             "One breach",
			duration:         "1.5",
			expectedBreaches: 1,
			totalRequests:    2,
		},
		{
			name:             "Another breach",
			duration:         "2.0",
			expectedBreaches: 2,
			totalRequests:    3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logRecord := createTestLogRecord()
			logRecord.Attributes().PutStr("duration", tt.duration)
			
			err := processor.processLog(ctx, logRecord)
			require.NoError(t, err)

			verifyMetrics(t, reader, tt.expectedBreaches, tt.totalRequests)
		})
	}
}

func TestConcurrentMetricUpdates(t *testing.T) {
	processor, reader := setupTestProcessor(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	numRequests := 10

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			logRecord := createTestLogRecord()
			logRecord.Attributes().PutStr("duration", "1.5") // Will cause breach
			
			err := processor.processLog(ctx, logRecord)
			require.NoError(t, err)
		}()
	}
	wg.Wait()

	verifyMetrics(t, reader, numRequests, numRequests)
}

// Mock S3 client
type mockS3Client struct {
	mock.Mock
}

func (m *mockS3Client) GetObject(ctx context.Context, input *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	args := m.Called(ctx, input, opts)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*s3.GetObjectOutput), args.Error(1)
}


func TestMergeSLOConfigs(t *testing.T) {
	processor, reader := setupTestProcessor(t)

	httpConfig := SLOConfig{
		SLOs: map[string]map[string]EndpointSLOConfig{
			"http": {
				"GET /test": {
					Latency: map[string]time.Duration{
						"p99": time.Second,
					},
				},
			},
		},
	}

	rpc2Config := SLOConfig{
		SLOs: map[string]map[string]EndpointSLOConfig{
			"rpc2": {
				"/service/method": {
					Latency: map[string]time.Duration{
						"p99": 500 * time.Millisecond,
					},
				},
			},
		},
	}

	merged := processor.mergeSLOConfigs(httpConfig, rpc2Config)
	
	assert.Contains(t, merged.SLOs, "http")
	assert.Contains(t, merged.SLOs, "rpc2")
	assert.Contains(t, merged.SLOs["http"], "GET /test")
	assert.Contains(t, merged.SLOs["rpc2"], "/service/method")

	// Verify no metrics were generated during merge
	verifyMetrics(t, reader, 0, 0)
}

func TestFetchSLOConfigFromS3(t *testing.T) {
    tests := []struct {
        name           string
        httpContent    string
        rpc2Content    string
        s3Error        error
        expectedError  bool
        setupMock      func(*mockS3Client)
        checkFiles     func(*testing.T, string, string)
    }{
        {
            name: "successful fetch",
            httpContent: `
slos:
  http:
    "GET /test":
      latency:
        "p99": "1s"
      success_rate: 99.9
`,
            rpc2Content: `
slos:
  rpc2:
    "/service/method":
      latency:
        "p95": "0.5s"
      success_rate: 99.5
`,
            setupMock: func(m *mockS3Client) {
                // Mock HTTP config response
                m.On("GetObject", mock.Anything, &s3.GetObjectInput{
                    Bucket: aws.String("test-bucket"),
                    Key:    aws.String("slos/slo_master_http.yaml"),
                }, mock.Anything).Return(&s3.GetObjectOutput{
                    Body: io.NopCloser(strings.NewReader(`
slos:
  http:
    "GET /test":
      latency:
        "p99": "1s"
      success_rate: 99.9
`)),
                }, nil)

                // Mock RPC2 config response
                m.On("GetObject", mock.Anything, &s3.GetObjectInput{
                    Bucket: aws.String("test-bucket"),
                    Key:    aws.String("slos/slo_master_rpc.yaml"),
                }, mock.Anything).Return(&s3.GetObjectOutput{
                    Body: io.NopCloser(strings.NewReader(`
slos:
  rpc2:
    "/service/method":
      latency:
        "p95": "0.5s"
      success_rate: 99.5
`)),
                }, nil)
            },
            checkFiles: func(t *testing.T, httpPath, rpc2Path string) {
                // Verify HTTP config file contents
                httpContent, err := os.ReadFile(httpPath)
				require.NoError(t, err)
                assert.Contains(t, string(httpContent), "GET /test")

                // Verify RPC2 config file contents
                rpc2Content, err := os.ReadFile(rpc2Path)
                require.NoError(t, err)
                assert.Contains(t, string(rpc2Content), "/service/method")
            },
        },
        {
            name:          "s3 fetch error",
            s3Error:       fmt.Errorf("S3 access denied"),
            expectedError: true,
            setupMock: func(m *mockS3Client) {
                m.On("GetObject", mock.Anything, mock.Anything, mock.Anything).
                    Return(nil, fmt.Errorf("S3 access denied"))
            },
            checkFiles: func(t *testing.T, httpPath, rpc2Path string) {
                // Files should not exist or be empty
                _, err := os.Stat(httpPath)
                assert.True(t, os.IsNotExist(err))
                _, err = os.Stat(rpc2Path)
                assert.True(t, os.IsNotExist(err))
            },
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // Create temp directory for test files
            tmpDir := t.TempDir()
            httpPath := filepath.Join(tmpDir, "slo_master_http.yaml")
            rpc2Path := filepath.Join(tmpDir, "slo_master_rpc.yaml")

            // Setup mock S3 client
            mockS3 := new(mockS3Client)
            tt.setupMock(mockS3)

            // Create processor
			proc, _ := setupTestProcessor(t)
            proc.s3Client = mockS3 // Inject the mock S3 client

            // Set environment variables
            t.Setenv("REGION", "us-east-1")

            // Call FetchSLOConfigFromS3
            err := proc.FetchSLOConfigFromS3(context.Background(), "test-bucket", httpPath, rpc2Path)

            if tt.expectedError {
                assert.Error(t, err)
            } else {
                assert.NoError(t, err)
                tt.checkFiles(t, httpPath, rpc2Path)
            }

            // Verify all mock expectations were met
            mockS3.AssertExpectations(t)
        })
    }
}

// Test for periodic refresh
func TestStartPeriodicRefresh(t *testing.T) {
    // Create temp directory for test files
    tmpDir := t.TempDir()
    httpPath := filepath.Join(tmpDir, "slo_master_http.yaml")
    rpc2Path := filepath.Join(tmpDir, "slo_master_rpc.yaml")

    // Setup mock S3 client
    mockS3 := new(mockS3Client)
    mockS3.On("GetObject", mock.Anything, mock.Anything, mock.Anything).
        Return(&s3.GetObjectOutput{
            Body: io.NopCloser(strings.NewReader(`slos: {}`)),
        }, nil)

    // Create processor
    proc, _ := setupTestProcessor(t)
    proc.s3Client = mockS3 // Inject the mock S3 client

    // Create context with timeout
    ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
    defer cancel()

    // Use a WaitGroup to ensure the goroutine finishes
    var wg sync.WaitGroup
    wg.Add(1)

    go func() {
        defer wg.Done()
        proc.startPeriodicRefresh(ctx, "test-bucket", httpPath, rpc2Path, 100*time.Millisecond)
    }()

    // Wait for some time to allow at least one refresh
    time.Sleep(300 * time.Millisecond)

    // Cancel the context and wait for the goroutine to finish
    cancel()
    wg.Wait()

    // Verify the mock S3 client was called at least once
    callCount := len(mockS3.Calls) // Get the actual number of calls
    assert.Greater(t, callCount, 0, "Expected GetObject to be called at least once")
}

func TestSLOMetricsProcessorConsumeLogs(t *testing.T) {
    tests := []struct {
        name             string
        attributes       map[string]string
        expectedBreaches int
        totalRequests    int
        shouldError      bool
    }{
        {
            name: "HTTP request within SLO",
            attributes: map[string]string{
                "method":                 "GET",
                "response_code":          "200",
                "content_type":           "application/json",
                "x_affirm_endpoint_name": "/test",  // Matches "GET /test" in config
                "duration":               "0.050",
            },
            expectedBreaches: 0,
            totalRequests:   1,
        },
        {
            name: "HTTP request breaching SLO",
            attributes: map[string]string{
                "method":                 "GET",
                "response_code":          "200",
                "content_type":           "application/json",
                "x_affirm_endpoint_name": "/test",
                "duration":               "1.500",  // Above 1s threshold
            },
            expectedBreaches: 1,
            totalRequests:   1,
        },
        {
            name: "RPC2 request breaching SLO",
            attributes: map[string]string{
                "method":        "POST",
                "response_code": "200",
                "content_type":  "application/rpc2",
                "path":         "/service/method",
                "duration":     "0.600",  // Above 500ms threshold
            },
            expectedBreaches: 1,
            totalRequests:   1,
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
            totalRequests:   1,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // Create processor with the exact configuration
            processor, reader := setupTestProcessor(t)
            logSink := new(consumertest.LogsSink)
            processor.nextConsumer = logSink

            require.NoError(t, processor.Start(context.Background(), componenttest.NewNopHost()))

            // Create and populate log record
            ld := plog.NewLogs()
            lr := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
            for k, v := range tt.attributes {
                lr.Attributes().PutStr(k, v)
            }

            // Process logs
            err := processor.ConsumeLogs(context.Background(), ld)
            if tt.shouldError {
                require.Error(t, err)
                return
            }
            require.NoError(t, err)
            require.Len(t, logSink.AllLogs(), 1)

            // Verify metrics
            verifyMetrics(t, reader, tt.expectedBreaches, tt.totalRequests)

            require.NoError(t, processor.Shutdown(context.Background()))
        })
    }
}