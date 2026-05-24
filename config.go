package dwbrowser

// Config holds configuration for the browser server.
type Config struct {
	Addr     string
	Headless bool
	PoolSize int
	DataDir  string
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Addr:     ":8033",
		Headless: true,
		PoolSize: 5,
		DataDir:  "",
	}
}

// Option is a functional option for Server.
type Option func(*Server)

// WithConfig sets the full Config.
func WithConfig(c Config) Option { return func(s *Server) { s.config = c } }

// WithHooks sets the Hooks.
func WithHooks(h Hooks) Option { return func(s *Server) { s.hooks = h } }

// WithAddr sets the listen address.
func WithAddr(addr string) Option { return func(s *Server) { s.config.Addr = addr } }

// WithHeadless sets headless mode.
func WithHeadless(v bool) Option { return func(s *Server) { s.config.Headless = v } }

// WithDataDir sets the data directory for browser profiles.
func WithDataDir(dir string) Option { return func(s *Server) { s.config.DataDir = dir } }
