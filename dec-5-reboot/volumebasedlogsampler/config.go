package volumebasedlogsampler

import "fmt"

type Config struct {
	DefaultSamplingRate float32  `mapstructure:"sampling_rate"`
	ExcludedEndpoints   []string `mapstructure:"excluded_endpoints"`
}

func (cfg *Config) Validate() error {
	if cfg.DefaultSamplingRate <= 0 || cfg.DefaultSamplingRate > 1 {
		return fmt.Errorf("invalid sampling rate: %f. Default sampling rate must be between 0 and 1", cfg.DefaultSamplingRate)
	}
	return nil
}
