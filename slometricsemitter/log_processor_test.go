package slometricsemitter

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
)

func setupTestProcessor(t *testing.T) *sloMetricsProcessor {
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

    return proc
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
	processor := setupTestProcessor(t)
	logRecord := createTestLogRecord()

	accessLog, err := processor.extractIstioAccessLogFromLogRecord(logRecord)
	require.NoError(t, err)

	assert.Equal(t, "http", accessLog.EndpointType)
	assert.Equal(t, "/test", accessLog.Endpoint)
	assert.Equal(t, int64(200), accessLog.StatusCode)
	assert.Equal(t, "GET", accessLog.Method)
	assert.Equal(t, 0.5, accessLog.Duration)
}

func TestDetermineEndpointType(t *testing.T) {
	processor := setupTestProcessor(t)
	tests := []struct {
		name        string
		contentType string
		expected    string
	}{
		{
			name:        "HTTP endpoint",
			contentType: "application/json",
			expected:    "http",
		},
		{
			name:        "RPC2 endpoint",
			contentType: "application/rpc2",
			expected:    "rpc2",
		},
		{
			name:        "gRPC endpoint",
			contentType: "application/grpc",
			expected:    "grpc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logRecord := createTestLogRecord()
			logRecord.Attributes().PutStr("content_type", tt.contentType)
			
			endpointType := processor.determineEndpointType(logRecord)
			assert.Equal(t, tt.expected, endpointType)
		})
	}
}

func TestExtractEndpoint(t *testing.T) {
	processor := setupTestProcessor(t)
	
	t.Run("HTTP endpoint", func(t *testing.T) {
		logRecord := createTestLogRecord()
		endpoint, err := processor.extractEndpoint(logRecord, "http")
		require.NoError(t, err)
		assert.Equal(t, "/test", endpoint)
	})

	t.Run("RPC2 endpoint", func(t *testing.T) {
		logRecord := createTestLogRecord()
		logRecord.Attributes().PutStr("path", "/service/method/extra")
		endpoint, err := processor.extractEndpoint(logRecord, "rpc2")
		require.NoError(t, err)
		assert.Equal(t, "/service/method", endpoint)
	})
}

func TestGetEndpointSLO(t *testing.T) {
	processor := setupTestProcessor(t)

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
}

func TestProcessLog(t *testing.T) {
	processor := setupTestProcessor(t)
	ctx := context.Background()

	t.Run("Process valid log", func(t *testing.T) {
		logRecord := createTestLogRecord()
		err := processor.processLog(ctx, logRecord)
		assert.NoError(t, err)
	})

	t.Run("Process log with missing attributes", func(t *testing.T) {
		logRecord := plog.NewLogRecord()
		err := processor.processLog(ctx, logRecord)
		assert.Error(t, err)
	})
}

func TestMergeSLOConfigs(t *testing.T) {
	processor := setupTestProcessor(t)

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
}

func TestEnsureFileExists(t *testing.T) {
	tempDir := t.TempDir()
	testFile := tempDir + "/test.yaml"
	
	logger := zap.NewNop()
	err := ensureFileExists(testFile, logger)
	require.NoError(t, err)
	
	_, err = os.Stat(testFile)
	assert.NoError(t, err)
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
            proc := setupTestProcessor(t)
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
    proc := setupTestProcessor(t)
    proc.s3Client = mockS3 // Inject the mock S3 client

    // Create context with timeout
    ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
    defer cancel()

    // Start periodic refresh with a shorter interval for testing
    go proc.startPeriodicRefresh(ctx, "test-bucket", httpPath, rpc2Path, 100*time.Millisecond)

    // Wait for some time to allow at least one refresh
    time.Sleep(300 * time.Millisecond)

    
    callCount := len(mockS3.Calls)  // Get the actual number of calls
    assert.Greater(t, callCount, 0, "Expected GetObject to be called at least once")
}