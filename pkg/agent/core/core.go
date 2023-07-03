package core

// The core scaling logic at the heart of the autoscaler-agent. This file implements everything with
// mostly pure-ish functions, so that all the making & receiving requests can be done elsewhere.
//
// Broadly our strategy is to mimic the kind of eventual consistency that is itself used in
// Kubernetes. The scaling logic wasn't always implemented like this, but because the
// autoscaler-agent *fundamentally* exists in an eventual consistency world, we have to either:
//  (a) make assumptions that we know are false; or
//  (b) design our system so it assumes less.
// We used to solve this by (a). We ran into¹ issues² going that way, because sometimes those false
// assumptions come back to haunt you.
//
// That said, there's still some tricky semantics we want to maintain. Internally, the
// autoscaler-agent must be designed around eventual consistency, but the API we expose to the
// vm-informant is strictly synchonous. As such, there's some subtle logic to make sure that we're
// not violating our own guarantees.
//
// ---
// ¹ https://github.com/neondatabase/autoscaling/issues/23
// ² https://github.com/neondatabase/autoscaling/issues/350

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// Config represents some of the static configuration underlying the decision-making of State
type Config struct {
	// DefaultScalingConfig is just copied from the global autoscaler-agent config.
	// If the VM's ScalingConfig is nil, we use this field instead.
	DefaultScalingConfig api.ScalingConfig

	// PluginRequestTick gives the period at which we should be making requests to the scheduler
	// plugin, even if nothing's changed.
	PluginRequestTick time.Duration

	// InformantDeniedDownscaleCooldown gives the time we must wait between making duplicate
	// downscale requests to the vm-informant where the previous failed.
	InformantDeniedDownscaleCooldown time.Duration

	// InformantRetryWait gives the amount of time to wait to retry after a *failed* request.
	InformantRetryWait time.Duration

	// Warn provides an outlet for (*State).Next() to give warnings about conditions that are
	// impeding its ability to execute. (e.g. "wanted to do X but couldn't because of Y")
	Warn func(string, ...any) `json:"-"`
}

// State holds all of the necessary internal state for a VM in order to make scaling
// decisions
type State struct {
	// ANY CHANGED FIELDS MUST BE UPDATED IN dump.go AS WELL

	mu sync.Mutex

	updateSignal util.CondChannelSender

	config Config

	// vm gives the current state of the VM - or at least, the state of the fields we care about.
	//
	// NB: any contents behind pointers in vm are immutable. Any time the field is updated, we
	// replace it with a fresh object.
	vm api.VmInfo

	// plugin records all state relevant to communications with the scheduler plugin
	plugin pluginState

	// informant records all state relevant to communications with the vm-informant
	informant informantState

	// neonvm records all state relevant to the NeonVM k8s API
	neonvm neonvmState

	metrics *api.Metrics
}

type pluginState struct {
	// ongoingRequest is true iff there is currently an ongoing request to *this* scheduler plugin.
	ongoingRequest bool
	// computeUnit, if not nil, gives the value of the compute unit we most recently got from a
	// PluginResponse
	computeUnit *api.Resources
	// lastRequest, if not nil, gives information about the most recently started request to the
	// plugin (maybe unfinished!)
	lastRequest *pluginRequested
	// permit, if not nil, stores the Permit in the most recent PluginResponse. This field will be
	// nil if we have not been able to contact *any* scheduler. If we switch schedulers, we trust
	// the old one.
	permit *api.Resources
}

type pluginRequested struct {
	at        time.Time
	resources api.Resources
}

type informantState struct {
	ongoingRequest bool

	// requestedUpscale, if not nil, stores the most recent *unresolved* upscaling requested by the
	// vm-informant, along with the time at which it occurred.
	requestedUpscale *requestedUpscale

	// deniedDownscale, if not nil, stores the result of the lastest denied /downscale request.
	deniedDownscale *deniedDownscale

	// approved stores the most recent Resources associated with either (a) an accepted downscale
	// request, or (b) a successful upscale notification.
	approved *api.Resources

	downscaleFailureAt *time.Time
	upscaleFailureAt   *time.Time
}

type requestedUpscale struct {
	at        time.Time
	base      api.Resources
	requested api.MoreResources
}

type deniedDownscale struct {
	at        time.Time
	requested api.Resources
}

type neonvmState struct {
	lastSuccess *api.Resources
	// ongoingRequested, if not nil, gives the resources requested
	ongoingRequested *api.Resources
	requestFailedAt  *time.Time
}

func NewState(vm api.VmInfo, config Config, notifyUpdates util.CondChannelSender) *State {
	return &State{
		mu:           sync.Mutex{},
		updateSignal: notifyUpdates,
		config:       config,
		vm:           vm,
		plugin: pluginState{
			ongoingRequest: false,
			computeUnit:    nil,
			lastRequest:    nil,
			permit:         nil,
		},
		informant: informantState{
			ongoingRequest:     false,
			requestedUpscale:   nil,
			deniedDownscale:    nil,
			approved:           nil,
			downscaleFailureAt: nil,
			upscaleFailureAt:   nil,
		},
		neonvm: neonvmState{
			lastSuccess:      nil,
			ongoingRequested: nil,
			requestFailedAt:  nil,
		},
		metrics: nil,
	}
}

// NextActions is used to implement the state machine. It's a pure function that *just* indicates
// what the executor should do.
func (s *State) NextActions(now time.Time) ActionSet {
	s.mu.Lock()
	defer s.mu.Unlock()

	var actions ActionSet

	desiredResources := s.desiredResourcesFromMetricsOrRequestedUpscaling()
	using := s.vm.Using()
	desiredResourcesApprovedByInformant := s.boundResourcesByInformantApproved(desiredResources)
	desiredResourcesApprovedByPlugin := s.boundResourcesByPluginApproved(desiredResources)
	// NB: informant approved provides a lower bound
	approvedDesiredResources := desiredResourcesApprovedByPlugin.Max(desiredResourcesApprovedByInformant)

	ongoingNeonVMRequest := s.neonvm.ongoingRequested != nil

	var requestForPlugin api.Resources
	if s.plugin.permit == nil {
		// If we haven't yet gotten a proper plugin response, then we aren't allowed to ask for
		// anything beyond our current usage.
		requestForPlugin = using
	} else {
		// ... Otherwise, we should:
		//  1. "inform" the plugin of any downscaling since the previous permit
		//  2. "request" any desired upscaling relative to to the previous permit
		// with (2) taking priority over (1), if there's any conflicts.
		requestForPlugin = desiredResources.Max(using) // ignore "desired" downscaling with .Max(using)
	}

	// We want to make a request to the scheduler plugin if:
	//  1. we've waited long enough since the previous request; or
	//  2.a. we want to request resources / inform it of downscale; and
	//    b. there isn't any ongoing, conflicting request
	timeForNewPluginRequest := s.plugin.lastRequest == nil || now.Sub(s.plugin.lastRequest.at) >= s.config.PluginRequestTick
	shouldUpdatePlugin := s.plugin.lastRequest != nil &&
		// "we haven't tried requesting *these* resources from it yet, or we can retry requesting"
		(s.plugin.lastRequest.resources != requestForPlugin || timeForNewPluginRequest) &&
		!ongoingNeonVMRequest

	if !s.plugin.ongoingRequest && (timeForNewPluginRequest || shouldUpdatePlugin) {
		if !shouldUpdatePlugin {
			// If we shouldn't "update" the plugin, then just inform it about the current resources
			// and metrics.
			actions.PluginRequest = &ActionPluginRequest{
				RequiresRequestLock: false,
				Resources:           using,
				Metrics:             s.metrics,
			}
		} else {
			// ... Otherwise, we should try requesting something new form it.
			actions.PluginRequest = &ActionPluginRequest{
				RequiresRequestLock: true, // true because this request is not just a heartbeat
				Resources:           desiredResourcesApprovedByInformant,
				Metrics:             s.metrics,
			}
		}
	} else if timeForNewPluginRequest || shouldUpdatePlugin {
		s.config.Warn("Wanted to make a request to the plugin, but there's already one ongoing")
	}

	// We want to make a request to NeonVM if we've been approved for a change in resources that
	// we're not currently using.
	if approvedDesiredResources != using {
		// ... but we can't make one if there's already a request ongoing, either via the NeonVM API
		// or to the scheduler plugin, because they require taking out the request lock.
		if !ongoingNeonVMRequest && !s.plugin.ongoingRequest {
			actions.NeonVMRequest = &ActionNeonVMRequest{Resources: approvedDesiredResources}
		} else {
			// prefer displaying there's a NeonVM request if there is one
			reqType := "plugin"
			if ongoingNeonVMRequest {
				reqType = "NeonVM"
			}

			s.config.Warn("Wanted to make a request to NeonVM API, but there's already a %s request ongoing", reqType)
		}
	}

	// We should make an upscale request to the informant if we've upscaled and the informant
	// doesn't know about it.
	wantInformantUpscaleRequest := s.informant.approved != nil && *s.informant.approved != desiredResources.Max(*s.informant.approved)
	// However, we may need to wait before retrying (or for any ongoing requests to finish)
	makeInformantUpscaleRequest := wantInformantUpscaleRequest &&
		!s.informant.ongoingRequest &&
		(s.informant.upscaleFailureAt == nil ||
			now.Sub(*s.informant.upscaleFailureAt) >= s.config.InformantRetryWait)
	if wantInformantUpscaleRequest {
		if makeInformantUpscaleRequest {
			actions.InformantUpscale = &ActionInformantUpscale{
				Resources: desiredResources.Max(*s.informant.approved),
			}
		} else if s.informant.ongoingRequest {
			s.config.Warn("Wanted to send informant downscale request, but waiting on other ongoing request")
		} else {
			s.config.Warn("Wanted to send informant downscale request, but waiting on retry rate limit")
		}
	}

	// We should make a downscale request to the informant if we want to downscale but haven't been
	// approved for it.
	var resourcesForInformantDownscale api.Resources
	if s.informant.approved != nil {
		resourcesForInformantDownscale = desiredResources.Min(*s.informant.approved)
	} else {
		resourcesForInformantDownscale = desiredResources.Min(using)
	}
	wantInformantDownscaleRequest := s.informant.approved != nil && *s.informant.approved != resourcesForInformantDownscale
	if s.informant.approved == nil && resourcesForInformantDownscale != using {
		s.config.Warn("Wanted to send informant downscale request, but haven't yet gotten information about its resources")
	}
	// However, we may need to wait before retrying (or for any ongoing requests to finish)
	makeInformantDownscaleRequest := wantInformantDownscaleRequest &&
		!s.informant.ongoingRequest &&
		(s.informant.deniedDownscale == nil ||
			s.informant.deniedDownscale.requested != desiredResources.Min(using) ||
			now.Sub(s.informant.deniedDownscale.at) >= s.config.InformantDeniedDownscaleCooldown) &&
		(s.informant.downscaleFailureAt == nil ||
			now.Sub(*s.informant.downscaleFailureAt) >= s.config.InformantRetryWait)

	if wantInformantDownscaleRequest {
		if makeInformantDownscaleRequest {
			actions.InformantDownscale = &ActionInformantDownscale{
				Target: resourcesForInformantDownscale,
			}
		} else if s.informant.ongoingRequest {
			s.config.Warn("Wanted to send informant downscale request, but waiting on other ongoing request")
		} else {
			s.config.Warn("Wanted to send informant downscale request, but waiting on retry rate limit")
		}
	}

	// --- and that's all the request types! ---

	// If there's anything waiting, we should also note how long we should wait for.
	// There's two components we could be waiting on: the scheduler plugin, and the vm-informant.
	maximumDuration := time.Duration(int64(uint64(1)<<63 - 1))
	requiredWait := maximumDuration

	// We always need to periodically send messages to the plugin. If actions.PluginRequest == nil,
	// we know that either:
	//
	//   (a) s.plugin.lastRequestAt != nil (otherwise timeForNewPluginRequest == true); or
	//   (b) s.plugin.ongoingRequest == true (the only reason why we wouldn't've exited earlier)
	//
	// So we actually only need to explicitly wait if there's not an ongoing request - otherwise
	// we'll be notified anyways when the request is done.
	if actions.PluginRequest == nil && !s.plugin.ongoingRequest {
		requiredWait = util.Min(requiredWait, now.Sub(s.plugin.lastRequest.at))
	}

	// For the vm-informant:
	// if we wanted to make EITHER a downscale or upscale request, but we previously couldn't
	// because of retry timeouts, we should wait for s.config.InformantRetryWait before trying
	// again.
	// OR if we wanted to downscale but got denied, we should wait for
	// s.config.InformantDownscaleCooldown before retrying.
	if !s.informant.ongoingRequest {
		// Retry upscale on failure
		if wantInformantUpscaleRequest && s.informant.upscaleFailureAt != nil {
			if wait := now.Sub(*s.informant.upscaleFailureAt); wait >= s.config.InformantRetryWait {
				requiredWait = util.Min(requiredWait, wait)
			}
		}
		// Retry downscale on failure
		if wantInformantDownscaleRequest && s.informant.downscaleFailureAt != nil {
			if wait := now.Sub(*s.informant.downscaleFailureAt); wait >= s.config.InformantRetryWait {
				requiredWait = util.Min(requiredWait, wait)
			}
		}
		// Retry downscale if denied
		if wantInformantDownscaleRequest && s.informant.deniedDownscale != nil && resourcesForInformantDownscale == s.informant.deniedDownscale.requested {
			if wait := now.Sub(s.informant.deniedDownscale.at); wait >= s.config.InformantDeniedDownscaleCooldown {
				requiredWait = util.Min(requiredWait, wait)
			}
		}
	}

	// If we're waiting on anything, add the action.
	if requiredWait != maximumDuration {
		actions.Wait = &ActionWait{Duration: requiredWait}
	}

	if err := actions.Validate(); err != nil {
		panic(fmt.Errorf("generated invalid ActionSet: %w", err))
	}

	return actions
}

func (s *State) scalingConfig() api.ScalingConfig {
	if s.vm.ScalingConfig != nil {
		return *s.vm.ScalingConfig
	} else {
		return s.config.DefaultScalingConfig
	}
}

func (s *State) desiredResourcesFromMetricsOrRequestedUpscaling() api.Resources {
	// There's some annoying edge cases that this function has to be able to handle properly. For
	// the sake of completeness, they are:
	//
	// 1. s.vm.Using() is not a multiple of s.computeUnit
	// 2. s.vm.Max() is less than s.computeUnit (or: has at least one resource that is)
	// 3. s.vm.Using() is a fractional multiple of s.computeUnit, but !allowDecrease and rounding up
	//    is greater than s.vm.Max()
	// 4. s.vm.Using() is much larger than s.vm.Min() and not a multiple of s.computeUnit, but load
	//    is low so we should just decrease *anyways*.
	//
	// ---
	//
	// Broadly, the implementation works like this:
	// 1. Based on load average, calculate the "goal" number of CPUs (and therefore compute units)
	// 2. Cap the goal CU by min/max, etc
	// 3. that's it!

	// If we don't know
	if s.plugin.computeUnit == nil {
		return s.vm.Using()
	}

	var goalCU uint32
	if s.metrics != nil {
		// Goal compute unit is at the point where (CPUs) × (LoadAverageFractionTarget) == (load
		// average),
		// which we can get by dividing LA by LAFT.
		goalCU = uint32(math.Round(float64(s.metrics.LoadAverage1Min) / s.scalingConfig().LoadAverageFractionTarget))
	}

	// Update goalCU based on any requested upscaling
	goalCU = util.Max(goalCU, s.requiredCUForRequestedUpscaling(*s.plugin.computeUnit))

	// resources for the desired "goal" compute units
	var goalResources api.Resources

	// If there's no constraints from s.metrics or s.informant.requestedUpscale, then we'd prefer to
	// keep things as-is, rather than scaling down (because otherwise goalCU = 0).
	if s.metrics == nil && s.informant.requestedUpscale == nil {
		goalResources = s.vm.Using()
	} else {
		goalResources = s.plugin.computeUnit.Mul(uint16(goalCU))
	}

	// bound goal by the minimum and maximum resource amounts for the VM
	result := goalResources.Min(s.vm.Max()).Max(s.vm.Min())

	// Check that the result is sound.
	//
	// With the current (naive) implementation, this is trivially ok. In future versions, it might
	// not be so simple, so it's good to have this integrity check here.
	if result.HasFieldGreaterThan(s.vm.Max()) {
		panic(fmt.Errorf(
			"produced invalid desiredVMState: result has field greater than max. this = %+v", s,
		))
	} else if result.HasFieldLessThan(s.vm.Min()) {
		panic(fmt.Errorf(
			"produced invalid desiredVMState: result has field less than min. this = %+v", s,
		))
	}

	return result
}

// NB: we could just use s.plugin.computeUnit, but that's sometimes nil. This way, it's clear that
// it's the caller's responsibility to ensure that s.plugin.computeUnit != nil.
func (s *State) requiredCUForRequestedUpscaling(computeUnit api.Resources) uint32 {
	if s.informant.requestedUpscale == nil {
		return 0
	}

	var required uint32
	requested := s.informant.requestedUpscale.requested

	// note: floor(x / M) + 1 gives the minimum integer value greater than x / M.

	if requested.Cpu {
		required = util.Max(required, uint32(s.vm.Cpu.Use/computeUnit.VCPU)+1)
	}
	if requested.Memory {
		required = util.Max(required, uint32(s.vm.Mem.Use/computeUnit.Mem)+1)
	}

	return required
}

func (s *State) boundResourcesByInformantApproved(resources api.Resources) api.Resources {
	var lowerBound api.Resources
	if s.informant.approved != nil {
		lowerBound = *s.informant.approved
	} else {
		lowerBound = s.vm.Using()
	}
	return resources.Max(lowerBound)
}

func (s *State) boundResourcesByPluginApproved(resources api.Resources) api.Resources {
	var upperBound api.Resources
	if s.plugin.permit != nil {
		upperBound = *s.plugin.permit
	} else {
		upperBound = s.vm.Using()
	}
	return resources.Min(upperBound)
}

//////////////////////////////////////////
// PUBLIC FUNCTIONS TO UPDATE THE STATE //
//////////////////////////////////////////

func (s *State) UpdatedVM(vm api.VmInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.vm = vm
	s.updateSignal.Send()
}

// PluginHandle provides write access to the scheduler plugin pieces of an UpdateState
type PluginHandle struct {
	s *State
}

func (s *State) Plugin() PluginHandle {
	return PluginHandle{s}
}

func (h PluginHandle) NewScheduler() {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()

	h.s.plugin = pluginState{
		ongoingRequest: false,
		computeUnit:    nil,
		lastRequest:    nil,
		permit:         h.s.plugin.permit, // Keep this; trust the previous scheduler.
	}
	h.s.updateSignal.Send()
}

func (h PluginHandle) StartingRequest(now time.Time, resources api.Resources) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()

	h.s.plugin.lastRequest = &pluginRequested{
		at:        now,
		resources: resources,
	}
	h.s.plugin.ongoingRequest = true
	h.s.updateSignal.Send()
}

func (h PluginHandle) RequestFailed() {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()

	h.s.plugin.ongoingRequest = false
	h.s.updateSignal.Send()
}

func (h *PluginHandle) ReceivedResponse(resp api.PluginResponse) error {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()

	h.s.plugin.ongoingRequest = false
	h.s.updateSignal.Send()

	if err := resp.Permit.ValidateNonZero(); err != nil {
		return fmt.Errorf("Invalid permit: %w", err)
	}
	if err := resp.ComputeUnit.ValidateNonZero(); err != nil {
		return fmt.Errorf("Invalid compute unit: %w", err)
	}

	// Errors from resp in connection with the prior request
	if resp.Permit.HasFieldGreaterThan(h.s.plugin.lastRequest.resources) {
		return fmt.Errorf(
			"Permit has resources greater than request (%+v vs. %+v)",
			resp.Permit, h.s.plugin.lastRequest.resources,
		)
	}

	// Errors from resp in connection with the prior request AND the VM state
	if vmUsing := h.s.vm.Using(); resp.Permit.HasFieldLessThan(vmUsing) {
		return fmt.Errorf("Permit has resources less than VM (%+v vs %+v)", resp.Permit, vmUsing)
	}

	// All good - set everything.

	h.s.plugin.computeUnit = &resp.ComputeUnit
	h.s.plugin.permit = &resp.Permit
	return nil
}

// InformantHandle provides write access to the vm-informant pieces of an UpdateState
type InformantHandle struct {
	s *State
}

func (s *State) Informant() InformantHandle {
	return InformantHandle{s}
}

func (h InformantHandle) Reset() {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()

	h.s.informant = informantState{
		ongoingRequest:     false,
		requestedUpscale:   nil,
		deniedDownscale:    nil,
		approved:           nil,
		downscaleFailureAt: nil,
		upscaleFailureAt:   nil,
	}
	h.s.updateSignal.Send()
}

func (h InformantHandle) SuccessfullyRegistered() {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()

	using := h.s.vm.Using()
	h.s.informant.approved = &using // TODO: this is racy (although... informant synchronization should help *some* with this?)
	h.s.updateSignal.Send()
}

func (h InformantHandle) UpscaleRequested(now time.Time, resources api.MoreResources) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()

	h.s.informant.requestedUpscale = &requestedUpscale{
		at:        now,
		base:      h.s.vm.Using(), // TODO: this is racy (maybe the resources were different when the informant originally made the request)
		requested: resources,
	}
	h.s.updateSignal.Send()
}

func (h InformantHandle) StartingUpscaleRequest() {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()

	h.s.informant.ongoingRequest = true
	h.s.informant.upscaleFailureAt = nil
	h.s.updateSignal.Send()
}

func (h InformantHandle) UpscaleRequestSuccess(resources api.Resources) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()

	h.s.informant.ongoingRequest = false
	h.s.informant.approved = &resources
	h.s.updateSignal.Send()
}

func (h InformantHandle) UpscaleRequestFailed(now time.Time) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()

	h.s.informant.ongoingRequest = false
	h.s.informant.upscaleFailureAt = &now
	h.s.updateSignal.Send()
}

func (h InformantHandle) StartingDownscaleRequest() {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()

	h.s.informant.ongoingRequest = true
	h.s.informant.downscaleFailureAt = nil
	h.s.updateSignal.Send()
}

func (h InformantHandle) DownscaleRequestAllowed(requested api.Resources) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()

	h.s.informant.ongoingRequest = false
	h.s.informant.approved = &requested
	h.s.informant.deniedDownscale = nil
	h.s.updateSignal.Send()
}

// Downscale request was successful but the informant denied our request.
func (h InformantHandle) DownscaleRequestDenied(now time.Time, requested api.Resources) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()

	h.s.informant.deniedDownscale = &deniedDownscale{
		at:        now,
		requested: requested,
	}
	h.s.updateSignal.Send()
}

func (h InformantHandle) DownscaleRequestFailed(now time.Time) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()

	h.s.informant.ongoingRequest = false
	h.s.informant.downscaleFailureAt = &now
	h.s.updateSignal.Send()
}

type NeonVMHandle struct {
	s *State
}

func (s *State) NeonVM() NeonVMHandle {
	return NeonVMHandle{s}
}

func (h NeonVMHandle) StartingRequest(resources api.Resources) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()

	h.s.neonvm.ongoingRequested = &resources
	h.s.updateSignal.Send()
}

func (h NeonVMHandle) RequestSuccessful() {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()

	if h.s.neonvm.ongoingRequested == nil {
		panic("received NeonVM().RequestSuccessful() update without ongoing request")
	}

	resources := *h.s.neonvm.ongoingRequested

	// FIXME: This is actually incorrect; we shouldn't trust that the VM has already been updated
	// just because the request completed. It takes longer for the reconcile cycle(s) to make the
	// necessary changes.
	h.s.vm.Cpu.Use = resources.VCPU
	h.s.vm.Mem.Use = resources.Mem
	h.s.updateSignal.Send()
}

func (h NeonVMHandle) RequestFailed() {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()

	h.s.neonvm.ongoingRequested = nil
	h.s.updateSignal.Send()
}
