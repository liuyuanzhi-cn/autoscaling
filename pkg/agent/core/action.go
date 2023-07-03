package core

import (
	"errors"
	"time"

	"github.com/neondatabase/autoscaling/pkg/api"
)

type ActionSet struct {
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

// Valid checks that the Action is valid. Currently, that at least one field is set
func (a ActionSet) Validate() error {
	hasField := a.Wait != nil ||
		a.PluginRequest != nil ||
		a.NeonVMRequest != nil ||
		a.InformantDownscale != nil ||
		a.InformantUpscale != nil

	if !hasField {
		return errors.New("No fields set")
	}

	return nil
}
