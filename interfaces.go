package veloxmiddleware

// Configurer is the interface for accessing RoadRunner configuration.
type Configurer interface {
	// Has checks if configuration section exists
	Has(name string) bool

	// UnmarshalKey unmarshals configuration section into target
	UnmarshalKey(name string, target interface{}) error
}

// Logger is the interface for RoadRunner logger.
type Logger interface {
	NamedLogger(name string) *zap.Logger
}
