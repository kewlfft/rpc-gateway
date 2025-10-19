package metrics

// Config defines the configuration for the metrics server
type Config struct {
	// Port to listen on for metrics endpoint (0 means disabled)
	Port uint `yaml:"port"`
}

// IsEnabled returns true if metrics server should be enabled
func (c Config) IsEnabled() bool {
	return c.Port > 0
}
