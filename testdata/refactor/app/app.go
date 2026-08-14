package app

import (
	"fmt"

	"example.com/refactor/base"
)

// Server embeds Config, so the implicit field is named Config.
type Server struct {
	base.Config
	addr string
}

// Namer is satisfied by base.Config via its String method.
type Namer interface {
	String() string
}

var (
	_ Namer        = base.Config{}
	_ fmt.Stringer = base.Config{}
)

func use(s Server) string {
	return s.Config.Name + s.String() + s.addr
}
