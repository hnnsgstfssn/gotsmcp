package base

// Config is embedded elsewhere and satisfies an interface.
type Config struct {
	Name string
}

func (c Config) String() string { return c.Name }

func (c Config) Rename() string { return c.Name }
