package demo

import (
	"fmt"

	"example.com/proj/other"
)

// Config carries demo settings.
type Config struct {
	Name string
}

// Load builds a Config.
func Load() *Config { return &Config{Name: "x"} }

// Server holds a field that happens to be named Config.
type Server struct {
	Config *Config
	addr   string
}

func (s *Server) Addr() string { return s.addr }

func run(s *Server) {
	Config := s.Config
	fmt.Println(Config, other.Config{}, other.Helper())
	_ = struct{ Config int }{}
}

func logf(format string, args ...any) { fmt.Printf(format, args...) }
