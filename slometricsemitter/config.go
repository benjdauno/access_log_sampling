package slometricsemitter

import (
	"fmt"
	"slices"
	// "strings"
	// "time"
)

type Config struct {
	// DefaultSamplingRate         float32 `mapstructure:"sampling_rate"`
	// ExcludedEndpointsConfigFile string  `mapstructure:"excluded_endpoints"`
	// PrometheusURL               string  `mapstructure:"prometheus_url"`
	Environment string `mapstructure:"environment"`
	// RefreshInterval             string  `mapstructure:"refresh_interval"`
	LogLevel string `mapstructure:"log_level"`
}

func (cfg *Config) Validate() error {
	// if cfg.DefaultSamplingRate <= 0 || cfg.DefaultSamplingRate > 1 {
	// 	return fmt.Errorf("invalid sampling rate: %f. Default sampling rate must be between 0 and 1", cfg.DefaultSamplingRate)
	// }
	// //Ensure he Prometheus URL starts with http:// or https://
	// if !strings.HasPrefix(cfg.PrometheusURL, "http://") && !strings.HasPrefix(cfg.PrometheusURL, "https://") {
	// 	return fmt.Errorf("invalid Prometheus URL: %s. URL must start with http:// or https://", cfg.PrometheusURL)
	// }

	if !slices.Contains([]string{"dev", "thor", "stage", "prod"}, cfg.Environment) {
		return fmt.Errorf("invalid environment: %s. Valid environments are dev, thor, stage, prod", cfg.Environment)
	}

	// //Ensure the refresh interval is a valid duration in golang time
	// if _, err := time.ParseDuration(cfg.RefreshInterval); err != nil {
	// 	return fmt.Errorf("invalid refresh interval: %s. Refresh interval must be a valid duration in golang time", cfg.RefreshInterval)
	// }
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, cfg.LogLevel) {
		return fmt.Errorf("invalid log level: %s. Valid log levels are debug, info, warn, error", cfg.LogLevel)
	}
	return nil

}
