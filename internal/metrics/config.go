package metrics

// Config defines the configuration for the metrics server
type Config struct {
	// Port to listen on for metrics endpoint
	Port uint `yaml:"port"`
}
