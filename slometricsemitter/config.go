package slometricsemitter

import (
	"fmt"
	"io"
	"os"
	"slices"

	"gopkg.in/yaml.v3"
)

type Config struct {
	SLOConfigFile string `mapstructure:"slo_config_file"`
	Environment   string `mapstructure:"environment"`
	LogLevel      string `mapstructure:"log_level"`
	// PrometheusURL               string  `mapstructure:"prometheus_url"`
	// RefreshInterval             string  `mapstructure:"refresh_interval"`
}

func (cfg *Config) Validate() error {
	if cfg.SLOConfigFile == "" {
		return fmt.Errorf("slo_config_file must be specified")
	}

	if !slices.Contains([]string{"dev", "thor", "stage", "prod"}, cfg.Environment) {
		return fmt.Errorf("invalid environment: %s. Valid environments are dev, thor, stage, prod", cfg.Environment)
	}

	if !slices.Contains([]string{"debug", "info", "warn", "error"}, cfg.LogLevel) {
		return fmt.Errorf("invalid log level: %s. Valid log levels are debug, info, warn, error", cfg.LogLevel)
	}

	// Check that slo_config_file exists and is not a directory
	_, err := os.Stat(cfg.SLOConfigFile)
	if err != nil {
		return fmt.Errorf("failed to stat slo_config_file at %q: %w", cfg.SLOConfigFile, err)
	}

	// Try opening and parsing the file as YAML
	f, err := os.Open(cfg.SLOConfigFile)
	if err != nil {
		return fmt.Errorf("failed to open slo_config_file at %q: %w", cfg.SLOConfigFile, err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("failed to read slo_config_file at %q: %w", cfg.SLOConfigFile, err)
	}

	var sloConfig SLOConfig
	if err := yaml.Unmarshal(data, &sloConfig); err != nil {
		return fmt.Errorf("slo_config_file %q is not in a valid format: %w", cfg.SLOConfigFile, err)
	}

	return nil
}
