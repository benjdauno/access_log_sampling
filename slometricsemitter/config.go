package slometricsemitter

import (
	"fmt"
	"io"
	"os"
	"slices"

	"gopkg.in/yaml.v3"
)

type Config struct {
	HTTPConfigFile   string `mapstructure:"http_slo_config_file"`
	RPC2ConfigFile   string `mapstructure:"rpc2_slo_config_file"`
	Environment      string `mapstructure:"environment"`
	LogLevel         string `mapstructure:"log_level"`
}

func (cfg *Config) Validate() error {
	// Validate HTTPConfigFile
	if cfg.HTTPConfigFile == "" {
		return fmt.Errorf("http_config_file must be specified")
	}
	if err := validateFile(cfg.HTTPConfigFile); err != nil {
		return fmt.Errorf("invalid HTTPConfigFile: %w", err)
	}

	// Validate RPC2ConfigFile
	if cfg.RPC2ConfigFile == "" {
		return fmt.Errorf("rpc2_config_file must be specified")
	}
	if err := validateFile(cfg.RPC2ConfigFile); err != nil {
		return fmt.Errorf("invalid RPC2ConfigFile: %w", err)
	}

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

// validateFile checks if the file exists, is not a directory, and is a valid YAML file
func validateFile(filePath string) error {
	// Check that the file exists and is not a directory
	_, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("failed to stat file at %q: %w", filePath, err)
	}

	// Try opening and parsing the file as YAML
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file at %q: %w", filePath, err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("failed to read file at %q: %w", filePath, err)
	}

	var config interface{}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("file %q is not in a valid YAML format: %w", filePath, err)
	}

	return nil
}
