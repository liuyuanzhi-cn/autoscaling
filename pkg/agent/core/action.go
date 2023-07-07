package core

import (
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
