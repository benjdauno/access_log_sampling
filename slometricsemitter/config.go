package slometricsemitter

import (
	"fmt"
	"slices"

)

type Config struct {
	Environment      string `mapstructure:"environment"`
	LogLevel         string `mapstructure:"log_level"`
}

func (cfg *Config) Validate() error {


	// Validate Environment
	if !slices.Contains([]string{"dev", "thor", "stage", "prod"}, cfg.Environment) {
		return fmt.Errorf("invalid environment: %s. Valid environments are dev, thor, stage, prod", cfg.Environment)
	}

	// Validate LogLevel
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, cfg.LogLevel) {
		return fmt.Errorf("invalid log level: %s. Valid log levels are debug, info, warn, error", cfg.LogLevel)
	}

	return nil
}
