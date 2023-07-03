package core

import (
	"errors"
	"time"

	"github.com/neondatabase/autoscaling/pkg/api"
)

type Action struct {
	Wait               *ActionWait               `json:"wait,omitempty"`
	PluginRequest      *ActionPluginRequest      `json:"pluginRequest,omitempty"`
	NeonVMRequest      *ActionNeonVMRequest      `json:"neonvmRequest,omitempty"`
	InformantDownscale *ActionInformantDownscale `json:"informantDownscale,omitempty"`
	InformantUpscale   *ActionInformantUpscale   `json:"informantUpscale,omitempty"`
}

type ActionWait struct {
	Duration time.Duration `json:"duration"`
}

type ActionPluginRequest struct {
	// FIXME: We should remove this. It's a lot of extra complication that no longer serves a
	// purpose.
	RequiresRequestLock bool `json:"requiresRequestLock"`

	Resources api.Resources `json:"resources"`
	Metrics   *api.Metrics  `json:"metrics"`
}

type ActionNeonVMRequest struct {
	Resources api.Resources `json:"resources"`
}

type ActionInformantDownscale struct {
	Target api.Resources `json:"target"`
}

type ActionInformantUpscale struct {
	Resources api.Resources `json:"resources"`
}

// Valid checks that the Action is valid. Currently, that exactly one field is set
func (a Action) Validate() error {
	fieldCount := 0
	if a.Wait != nil {
		fieldCount += 1
	}
	if a.InformantDownscale != nil {
		fieldCount += 1
	}
	if a.InformantUpscale != nil {
		fieldCount += 1
	}
	if a.PluginRequest != nil {
		fieldCount += 1
	}

	switch fieldCount {
	case 0:
		return errors.New("No fields set")
	case 1:
		return nil
	default:
		return errors.New("More than one field set")
	}
}
