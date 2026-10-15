package controllerruntime

import (
	"github.com/kerolasa/teleport-sre/internal"
)

// The configuration is the one every binary of the repository reads, so it is
// spelled out once next to the code that is shared with them.
type (
	Conf     = internal.Conf
	Listener = internal.Listener
	TLSConf  = internal.TLSConf
	KubeConf = internal.KubeConf
)
