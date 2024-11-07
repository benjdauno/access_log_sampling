package volumebasedlogsampler

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
)

type volumeBasedLogSamplerProcessor struct {
	host         component.Host
	cancel       context.CancelFunc
	logger       *zap.Logger
	nextConsumer consumer.Logs
	config       *Config
}

// Capabilities implements processor.Logs.
func (volBLogProc *volumeBasedLogSamplerProcessor) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{
		MutatesData: false,
	}
}

// ConsumeLogs implements processor.Logs.
// ConsumeLogs implements processor.Logs.
func (volBLogProc *volumeBasedLogSamplerProcessor) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	// Get the ResourceLogs slice from the plog.Logs object
	for i := 0; i < ld.ResourceLogs().Len(); i++ {
		resourceLogs := ld.ResourceLogs().At(i)
		// Iterate over the ScopeLogs within each ResourceLogs
		for j := 0; j < resourceLogs.ScopeLogs().Len(); j++ {
			scopeLogs := resourceLogs.ScopeLogs().At(j)
			// Iterate over each log record within ScopeLogs
			for k := 0; k < scopeLogs.LogRecords().Len(); k++ {
				logRecord := scopeLogs.LogRecords().At(k)
				// Log information about the log entry
				volBLogProc.logger.Info("Processing log entry",
					zap.String("log_message", logRecord.Body().AsString()),
					zap.Time("timestamp", logRecord.Timestamp().AsTime()),
				)
			}
		}
	}
	// Forward the logs to the next consumer
	return volBLogProc.nextConsumer.ConsumeLogs(ctx, ld)
}

func (volBLogProc *volumeBasedLogSamplerProcessor) Start(ctx context.Context, host component.Host) error {
	volBLogProc.host = host
	ctx = context.Background()
	ctx, volBLogProc.cancel = context.WithCancel(ctx)
	return nil
}

// func (volBLogProc *volumeBasedLogSamplerProcessor) Capabilities()
func (volBLogProc *volumeBasedLogSamplerProcessor) Shutdown(ctx context.Context) error {
	if volBLogProc.cancel != nil {
		volBLogProc.cancel()
	}
	return nil
}
