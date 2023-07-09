package executor

// Consumers of pkg/agent/core, implementing the "executors" for each type of action. These are
// wrapped up into a single ExecutorCore type, which exposes some methods for the various executors.
//
// The executors use various abstract interfaces for the scheudler / NeonVM / informant. The
// implementations of those interfaces are defiend in ifaces.go

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type Config = core.Config

type ExecutorCore struct {
	mu sync.Mutex

	core    *core.State
	actions *timedActions

	updates *util.Broadcaster
}

type ClientSet struct {
	Plugin    PluginInterface
	NeonVM    NeonVMInterface
	Informant InformantInterface
}

func NewExecutorCore(vm api.VmInfo, config core.Config) *ExecutorCore {
	return &ExecutorCore{
		mu:      sync.Mutex{},
		core:    core.NewState(vm, config),
		updates: util.NewBroadcaster(),
	}
}

type ExecutorCoreWithClients struct {
	*ExecutorCore

	clients ClientSet
}

func (c *ExecutorCore) WithClients(clients ClientSet) ExecutorCoreWithClients {
	return ExecutorCoreWithClients{
		ExecutorCore: c,
		clients:      clients,
	}
}

type timedActions struct {
	calculatedAt time.Time
	actions      core.ActionSet
}

func (c *ExecutorCore) getActions() timedActions {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.actions == nil {
		// NOTE: Even though we cache the actions generated using time.Now(), it's *generally* ok.
		now := time.Now()
		c.actions = &timedActions{calculatedAt: now, actions: c.core.NextActions(now)}
	}

	return *c.actions
}

func (c *ExecutorCore) update(with func(*core.State)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// NB: We broadcast the update *before* calling with() because this gets us nicer ordering
	// guarantees in some cases.
	c.updates.Broadcast()
	c.actions = nil
	with(c.core)
}

func (c *ExecutorCore) DoSleeper(ctx context.Context, logger *zap.Logger) {
	updates := c.updates.NewReceiver()

	// preallocate the timer. We clear it at the top of the loop; the 0 duration is just because we
	// need *some* value, so it might as well be zero.
	timer := time.NewTimer(0)
	defer timer.Stop()

	last := c.getActions()
	for {
		// Ensure the timer is cleared at the top of the loop
		if !timer.Stop() {
			<-timer.C
		}

		// If NOT waiting for a particular duration:
		if last.actions.Wait == nil {
			select {
			case <-ctx.Done():
				return
			case <-updates.Wait():
				updates.Awake()
				last = c.getActions()
			}
		}

		// If YES waiting for a particular duration
		if last.actions.Wait != nil {
			// NB: It's possible for last.calculatedAt to be somewhat out of date. It's *probably*
			// fine, because we'll be given a notification any time the state has changed, so we
			// should wake from a select soon enough to get here
			timer.Reset(last.actions.Wait.Duration)

			select {
			case <-ctx.Done():
				return
			case <-updates.Wait():
				updates.Awake()

				last = c.getActions()
			case <-timer.C:
				select {
				// If there's also an update, then let that take preference:
				case <-updates.Wait():
					updates.Awake()
					last = c.getActions()
				// Otherwise, trigger cache invalidation because we've waited for the requested
				// amount of time:
				default:
					c.update(func(*core.State) {})
					updates.Awake()
					last = c.getActions()
				}
			}
		}
	}
}

type PluginInterface interface {
	RequestLock() util.ChanMutex
	NewPlugin() util.CondChannelReceiver
	GetHandle() PluginHandle
}

type PluginHandle interface {
	Request(_ context.Context, _ *zap.Logger, lastPermit *api.Resources, target api.Resources, _ *api.Metrics) (*api.PluginResponse, error)
}

func (c *ExecutorCoreWithClients) DoPluginRequests(ctx context.Context, logger *zap.Logger) {
	var (
		updates     util.BroadcastReceiver   = c.updates.NewReceiver()
		requestLock util.ChanMutex           = c.clients.Plugin.RequestLock()
		newPlugin   util.CondChannelReceiver = c.clients.Plugin.NewPlugin()
		ifaceLogger *zap.Logger              = logger.Named("client")
	)

	holdingRequestLock := false
	releaseRequestLockIfHolding := func() {
		if holdingRequestLock {
			requestLock.Unlock()
			holdingRequestLock = false
		}
	}
	defer releaseRequestLockIfHolding()

	resetScheduler := func(state *core.State) {
		state.Plugin().NewScheduler()
	}

	last := c.getActions()
	for {
		releaseRequestLockIfHolding()

		// Always receive an update if there is one. This helps with reliability (better guarantees
		// about not missing updates) and means that the switch statements can be simpler.
		select {
		case <-updates.Wait():
			updates.Awake()
			last = c.getActions()
		default:
		}

		// Wait until we're supposed to make a request.
		if last.actions.PluginRequest == nil {
			select {
			case <-ctx.Done():
				return
			case <-newPlugin.Recv():
				c.update(resetScheduler)
				continue
			case <-updates.Wait():
				// NB: don't .Awake(); allow that to be handled at the top of the loop.
				continue
			}
		}

		action := *last.actions.PluginRequest

		pluginIface := c.clients.Plugin.GetHandle()

		// Make sure we always handle any trailing newPlugin event
		select {
		case <-newPlugin.Recv():
			c.update(resetScheduler)
			continue
		default:
		}

		// Try to acquire the request lock, but if something happens while we're waiting, we'll
		// abort & retry on the next loop iteration (or maybe not, if last.actions changed).
		select {
		case <-ctx.Done():
			return
		case <-newPlugin.Recv():
			c.update(resetScheduler)
			continue
		case <-updates.Wait():
			// NB: don't .Awake(); allow that to be handled at the top of the loop.
			continue
		case <-requestLock.WaitLock():
			holdingRequestLock = true
		}

		// Update the state to indicate that the request is starting.
		var startTime time.Time
		c.update(func(state *core.State) {
			startTime = time.Now()
			state.Plugin().StartingRequest(startTime, action.Target)
		})

		if pluginIface == nil {
			c.update(func(state *core.State) {
				// FIXME: log error
				state.Plugin().RequestFailed()
			})
			continue
		}

		resp, err := pluginIface.Request(ctx, ifaceLogger, action.LastPermit, action.Target, action.Metrics)
		// endTime := time.Now()
		c.update(func(state *core.State) {
			if err != nil {
				// FIXME: log error
				state.Plugin().RequestFailed()
			} else {
				// FIXME: log success
				if err := state.Plugin().RequestSuccessful(*resp); err != nil {
					// FIXME: log validation error
				}
			}
		})
	}
}

type NeonVMInterface interface {
	RequestLock() util.ChanMutex
	Request(_ context.Context, _ *zap.Logger, current, target api.Resources) error
}

func (c *ExecutorCoreWithClients) DoNeonVMRequests(ctx context.Context, logger *zap.Logger) {
	var (
		updates     util.BroadcastReceiver = c.updates.NewReceiver()
		requestLock util.ChanMutex         = c.clients.NeonVM.RequestLock()
		ifaceLogger *zap.Logger            = logger.Named("client")
	)

	holdingRequestLock := false
	releaseRequestLockIfHolding := func() {
		if holdingRequestLock {
			requestLock.Unlock()
			holdingRequestLock = false
		}
	}
	defer releaseRequestLockIfHolding()

	last := c.getActions()
	for {
		releaseRequestLockIfHolding()

		// Always receive an update if there is one. This helps with reliability (better guarantees
		// about not missing updates) and means that the switch statements can be simpler.
		select {
		case <-updates.Wait():
			updates.Awake()
			last = c.getActions()
		default:
		}

		// Wait until we're supposed to make a request.
		if last.actions.NeonVMRequest == nil {
			select {
			case <-ctx.Done():
				return
			case <-updates.Wait():
				// NB: don't .Awake(); allow that to be handled at the top of the loop.
				continue
			}
		}

		action := *last.actions.NeonVMRequest

		// Try to acquire the request lock, but if something happens while we're waiting, we'll
		// abort & retry on the next loop iteration (or maybe not, if last.actions changed).
		select {
		case <-ctx.Done():
			return
		case <-updates.Wait():
			// NB: don't .Awake(); allow that to be handled at the top of the loop.
			continue
		case <-requestLock.WaitLock():
			holdingRequestLock = true
		}

		var startTime time.Time
		c.update(func(state *core.State) {
			startTime = time.Now()
			state.NeonVM().StartingRequest(startTime, action.Target)
		})

		err := c.clients.NeonVM.Request(ctx, ifaceLogger, action.Current, action.Target)
		c.update(func(state *core.State) {
			if err != nil {
				// FIXME: log error
				state.NeonVM().RequestFailed()
				return
			} else /* err == nil */ {
				// FIXME: log success
				state.NeonVM().RequestSuccessful()
			}
		})
	}
}

type InformantInterface interface {
	RequestLock() util.ChanMutex
	Downscale(_ context.Context, _ *zap.Logger, current, target api.Resources) (*api.DownscaleResult, error)
	Upscale(_ context.Context, _ *zap.Logger, current, target api.Resources) error
}

func (c *ExecutorCoreWithClients) DoInformantDownscales(ctx context.Context, logger *zap.Logger) {
	var (
		updates     util.BroadcastReceiver = c.updates.NewReceiver()
		requestLock util.ChanMutex         = c.clients.Informant.RequestLock()
		ifaceLogger *zap.Logger            = logger.Named("client")
	)

	holdingRequestLock := false
	releaseRequestLockIfHolding := func() {
		if holdingRequestLock {
			requestLock.Unlock()
			holdingRequestLock = false
		}
	}
	defer releaseRequestLockIfHolding()

	last := c.getActions()
	for {
		releaseRequestLockIfHolding()
		// re-fetch the request lock on every loop iteration, because each instance of communication
		// with the informant will have its own request lock.
		requestLock = c.clients.Informant.RequestLock()

		// Always receive an update if there is one. This helps with reliability (better guarantees
		// about not missing updates) and means that the switch statements can be simpler.
		select {
		case <-updates.Wait():
			updates.Awake()
			last = c.getActions()
		default:
		}

		// Wait until we're supposed to make a request.
		if last.actions.InformantDownscale == nil {
			select {
			case <-ctx.Done():
				return
			case <-updates.Wait():
				// NB: don't .Awake(); allow that to be handled at the top of the loop.
				continue
			}
		}

		action := *last.actions.InformantDownscale

		// Try to acquire the request lock, but if something happens while we're waiting, we'll
		// abort & retry on the next loop iteration (or maybe not, if last.actions changed).
		select {
		case <-ctx.Done():
			return
		case <-updates.Wait():
			// NB: don't .Awake(); allow that to be handled at the top of the loop.
			continue
		case <-requestLock.WaitLock():
			holdingRequestLock = true
		}

		// var startTime time.Time
		c.update(func(state *core.State) {
			// startTime = time.Now()
			state.Informant().StartingDownscaleRequest()
		})

		result, err := c.clients.Informant.Downscale(ctx, ifaceLogger, action.Current, action.Target)
		endTime := time.Now()

		c.update(func(state *core.State) {
			if err != nil {
				// FIXME: log error
				state.Informant().DownscaleRequestFailed(endTime)
				return
			}

			// FIXME: log request success
			if !result.Ok {
				// FIXME: warn not ok
				state.Informant().DownscaleRequestDenied(endTime, action.Target)
			} else {
				// FIXME: log approval
				state.Informant().DownscaleRequestAllowed(action.Target)
			}
		})
	}
}

func (c *ExecutorCoreWithClients) DoInformantUpscales(ctx context.Context, logger *zap.Logger) {
	var (
		updates     util.BroadcastReceiver = c.updates.NewReceiver()
		requestLock util.ChanMutex         = c.clients.Informant.RequestLock()
		ifaceLogger *zap.Logger            = logger.Named("client")
	)

	holdingRequestLock := false
	releaseRequestLockIfHolding := func() {
		if holdingRequestLock {
			requestLock.Unlock()
			holdingRequestLock = false
		}
	}
	defer releaseRequestLockIfHolding()

	last := c.getActions()
	for {
		releaseRequestLockIfHolding()
		// re-fetch the request lock on every loop iteration, because each instance of communication
		// with the informant will have its own request lock.
		requestLock = c.clients.Informant.RequestLock()

		// Always receive an update if there is one. This helps with reliability (better guarantees
		// about not missing updates) and means that the switch statements can be simpler.
		select {
		case <-updates.Wait():
			updates.Awake()
			last = c.getActions()
		default:
		}

		// Wait until we're supposed to make a request.
		if last.actions.InformantUpscale == nil {
			select {
			case <-ctx.Done():
				return
			case <-updates.Wait():
				// NB: don't .Awake(); allow that to be handled at the top of the loop.
				continue
			}
		}

		action := *last.actions.InformantUpscale

		// Try to acquire the request lock, but if something happens while we're waiting, we'll
		// abort & retry on the next loop iteration (or maybe not, if last.actions changed).
		select {
		case <-ctx.Done():
			return
		case <-updates.Wait():
			// NB: don't .Awake(); allow that to be handled at the top of the loop.
			continue
		case <-requestLock.WaitLock():
			holdingRequestLock = true
		}

		// var startTime time.Time
		c.update(func(state *core.State) {
			// startTime = time.Now()
			state.Informant().StartingDownscaleRequest()
		})

		err := c.clients.Informant.Upscale(ctx, ifaceLogger, action.Current, action.Target)
		endTime := time.Now()

		c.update(func(state *core.State) {
			if err != nil {
				// FIXME: log error
				state.Informant().UpscaleRequestFailed(endTime)
				return
			}

			// FIXME: log request success
			state.Informant().UpscaleRequestSuccessful(action.Target)
		})
	}
}
