package core

// Implementation of (*UpdateState).Dump()

import (
	"time"

	"github.com/neondatabase/autoscaling/pkg/api"
)

func shallowCopy[T any](ptr *T) *T {
	if ptr == nil {
		return nil
	} else {
		x := *ptr
		return &x
	}
}

// UpdateStateDump provides introspection into the current values of the fields of UpdateState
type UpdateStateDump struct {
	Config    Config             `json:"config"`
	VM        api.VmInfo         `json:"vm"`
	Plugin    pluginStateDump    `json:"plugin"`
	Informant informantStateDump `json:"informant"`
	NeonVM    neonvmStateDump    `json:"neonvm"`
	Metrics   *api.Metrics       `json:"metrics"`
}

// Dump produces a JSON-serializable representation of the UpdateState
func (s *State) Dump() UpdateStateDump {
	s.mu.Lock()
	defer s.mu.Unlock()

	return UpdateStateDump{
		Config:    s.config,
		VM:        s.vm,
		Plugin:    s.plugin.dump(),
		Informant: s.informant.dump(),
		NeonVM:    s.neonvm.dump(),
		Metrics:   shallowCopy(s.metrics),
	}
}

type pluginStateDump struct {
	OngoingRequest bool                 `json:"ongoingRequest"`
	ComputeUnit    *api.Resources       `json:"computeUnit"`
	LastRequest    *pluginRequestedDump `json:"lastRequest"`
	Permit         *api.Resources       `json:"permit"`
}
type pluginRequestedDump struct {
	At        time.Time     `json:"time"`
	Resources api.Resources `json:"resources"`
}

func (s *pluginState) dump() pluginStateDump {
	var lastRequest *pluginRequestedDump
	if s.lastRequest != nil {
		lastRequest = &pluginRequestedDump{
			At:        s.lastRequest.at,
			Resources: s.lastRequest.resources,
		}
	}

	return pluginStateDump{
		OngoingRequest: s.ongoingRequest,
		ComputeUnit:    shallowCopy(s.computeUnit),
		LastRequest:    lastRequest,
		Permit:         shallowCopy(s.permit),
	}
}

type informantStateDump struct {
	OngoingRequest     bool                  `json:"ongoingRequest"`
	RequestedUpscale   *requestedUpscaleDump `json:"requestedUpscale"`
	DeniedDownscale    *deniedDownscaleDump  `json:"deniedDownscale"`
	Approved           *api.Resources        `json:"approved"`
	DownscaleFailureAt *time.Time            `json:"downscaleFailureAt"`
	UpscaleFailureAt   *time.Time            `json:"upscaleFailureAt"`
}
type requestedUpscaleDump struct {
	At        time.Time         `json:"at"`
	Base      api.Resources     `json:"base"`
	Requested api.MoreResources `json:"requested"`
}
type deniedDownscaleDump struct {
	At        time.Time     `json:"at"`
	Requested api.Resources `json:"requested"`
}

func (s *informantState) dump() informantStateDump {
	var requestedUpscale *requestedUpscaleDump
	if s.requestedUpscale != nil {
		requestedUpscale = &requestedUpscaleDump{
			At:        s.requestedUpscale.at,
			Base:      s.requestedUpscale.base,
			Requested: s.requestedUpscale.requested,
		}
	}

	var deniedDownscale *deniedDownscaleDump
	if s.deniedDownscale != nil {
		deniedDownscale = &deniedDownscaleDump{
			At:        s.deniedDownscale.at,
			Requested: s.deniedDownscale.requested,
		}
	}

	return informantStateDump{
		OngoingRequest:     s.ongoingRequest,
		RequestedUpscale:   requestedUpscale,
		DeniedDownscale:    deniedDownscale,
		Approved:           shallowCopy(s.approved),
		DownscaleFailureAt: shallowCopy(s.downscaleFailureAt),
		UpscaleFailureAt:   shallowCopy(s.upscaleFailureAt),
	}
}

type neonvmStateDump struct {
	LastSuccess      *api.Resources `json:"lastSuccess"`
	OngoingRequested *api.Resources `json:"ongoingRequested"`
	RequestFailedAt  *time.Time     `json:"requestFailedAt"`
}

func (s *neonvmState) dump() neonvmStateDump {
	return neonvmStateDump{
		LastSuccess:      shallowCopy(s.lastSuccess),
		OngoingRequested: shallowCopy(s.ongoingRequested),
		RequestFailedAt:  shallowCopy(s.requestFailedAt),
	}
}
