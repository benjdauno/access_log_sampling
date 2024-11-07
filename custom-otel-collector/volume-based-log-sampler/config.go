package volumebasedlogsampler

type Config struct {
	SamplingRate      float32  `mapstructure:"sampling_rate"`
	ExcludedEndpoints []string `mapstructure:"excluded_endpoints"`
}
