package core

import (
	"github.com/wandering-compiler/w17ctl/internal/devconfig"
)

// LoadDevConfigFn / SaveDevConfigFn are the dev-machine-local project
// registry (~/.w17/config.yaml) seams — production uses the real
// devconfig implementations; tests override to point at a temp config.
// Shared by init (project registration), project, stack, and initiative.
var (
	LoadDevConfigFn = devconfig.LoadDefault
	SaveDevConfigFn = devconfig.SaveDefault
)
