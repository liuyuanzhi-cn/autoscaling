package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tychoish/fun/srv"

	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/api"
)

const (
	MaxHTTPBodySize  int64  = 1 << 10 // 1 KiB
	ContentTypeJSON  string = "application/json"
	ContentTypeError string = "text/plain"
)

// The scheduler plugin currently supports v1.0 to v1.1 of the agent<->scheduler plugin protocol.
//
// If you update either of these values, make sure to also update VERSIONING.md.
const (
	MinPluginProtocolVersion api.PluginProtoVersion = api.PluginProtoV1_0
	MaxPluginProtocolVersion api.PluginProtoVersion = api.PluginProtoV1_1
)

// startPermitHandler runs the server for handling each resourceRequest from a pod
func (e *AutoscaleEnforcer) startPermitHandler(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte("must be POST"))
			return
		}

		defer r.Body.Close()
		var req api.AgentRequest
		jsonDecoder := json.NewDecoder(io.LimitReader(r.Body, MaxHTTPBodySize))
		if err := jsonDecoder.Decode(&req); err != nil {
			klog.Warningf("[autoscale-enforcer] Received bad JSON request: %s", err)
			w.Header().Add("Content-Type", ContentTypeError)
			w.WriteHeader(400)
			_, _ = w.Write([]byte("bad JSON"))
			return
		}

		klog.Infof(
			"[autoscale-enforcer] Received autoscaler-agent request (client = %s) %+v",
			r.RemoteAddr, req,
		)
		resp, statusCode, err := e.handleAgentRequest(req)
		if err != nil {
			msg := fmt.Sprintf(
				"[autoscale-enforcer] Responding with status code %d to pod %v: %s",
				statusCode, req.Pod, err,
			)
			if 500 <= statusCode && statusCode < 600 {
				klog.Errorf("%s", msg)
			} else {
				klog.Warningf("%s", msg)
			}
			w.Header().Add("Content-Type", ContentTypeError)
			w.WriteHeader(statusCode)
			_, _ = w.Write([]byte(err.Error()))
			return
		}

		responseBody, err := json.Marshal(&resp)
		if err != nil {
			klog.Fatalf("Error encoding response JSON: %s", err)
		}

		w.Header().Add("Content-Type", ContentTypeJSON)
		w.WriteHeader(statusCode)
		_, _ = w.Write(responseBody)
	})

	orca := srv.GetOrchestrator(ctx)

	klog.Info("[autoscale-enforcer] Starting resource request server")
	hs := srv.HTTP("resource-request", 5*time.Second, &http.Server{Addr: "0.0.0.0:10299", Handler: mux})
	if err := hs.Start(ctx); err != nil {
		return fmt.Errorf("Error starting resource request server: %w", err)
	}

	if err := orca.Add(hs); err != nil {
		return fmt.Errorf("Error adding resource request server to orchestrator: %w", err)
	}
	return nil
}

// Returns body (if successful), status code, error (if unsuccessful)
func (e *AutoscaleEnforcer) handleAgentRequest(req api.AgentRequest) (*api.PluginResponse, int, error) {
	// Before doing anything, check that the version is within the range we're expecting.
	expectedProtoRange := api.VersionRange[api.PluginProtoVersion]{
		Min: MinPluginProtocolVersion,
		Max: MaxPluginProtocolVersion,
	}

	if !req.ProtoVersion.IsValid() {
		return nil, 400, fmt.Errorf("Invalid protocol version %v", req.ProtoVersion)
	}
	reqProtoRange := req.ProtocolRange()
	if _, ok := expectedProtoRange.LatestSharedVersion(reqProtoRange); !ok {
		return nil, 400, fmt.Errorf(
			"Protocol version mismatch: Need %v but got %v", expectedProtoRange, reqProtoRange,
		)
	}

	// if req.Metrics is nil, check that the protocol version allows that.
	if req.Metrics == nil && !req.ProtoVersion.AllowsNilMetrics() {
		return nil, 400, fmt.Errorf("nil metrics not supported for protocol version %v", req.ProtoVersion)
	}

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	pod, ok := e.state.podMap[req.Pod]
	if !ok {
		klog.Warningf("[autoscale-enforcer] Received request for pod we don't know: %+v", req)
		return nil, 404, errors.New("pod not found")
	}

	node := pod.node

	mustMigrate := pod.migrationState == nil &&
		// Check whether the pod *will* migrate, then update its resources, and THEN start its
		// migration, using the possibly-changed resources.
		e.updateMetricsAndCheckMustMigrate(pod, node, req.Metrics) &&
		// Don't migrate if it's disabled
		e.state.conf.migrationEnabled()

	permit, status, err := e.handleResources(pod, node, req.Resources, mustMigrate)
	if err != nil {
		return nil, status, err
	}

	var migrateDecision *api.MigrateResponse
	if mustMigrate {
		migrateDecision = &api.MigrateResponse{}
		err = e.state.startMigration(context.Background(), pod, e.vmClient)
		if err != nil {
			return nil, 500, fmt.Errorf("Error starting migration for pod %v: %w", pod.name, err)
		}
	}

	resp := api.PluginResponse{
		Permit:      permit,
		Migrate:     migrateDecision,
		ComputeUnit: *node.computeUnit,
	}
	pod.mostRecentComputeUnit = node.computeUnit // refer to common allocation
	return &resp, 200, nil
}

func (e *AutoscaleEnforcer) handleResources(
	pod *podState,
	node *nodeState,
	req api.Resources,
	startingMigration bool,
) (api.Resources, int, error) {
	milliVCPUs := FromResourceQuantity(req.VCPU)
	// Check that we aren't being asked to do something during migration:
	if pod.currentlyMigrating() {
		// The agent shouldn't have asked for a change after already receiving notice that it's
		// migrating.
		if milliVCPUs != pod.vCPU.Reserved || req.Mem != pod.memSlots.Reserved {
			err := errors.New("cannot change resources: agent has already been informed that pod is migrating")
			return api.Resources{}, 400, err
		}
		return api.Resources{VCPU: pod.vCPU.Reserved.toResourceQuantity(), Mem: pod.memSlots.Reserved}, 200, nil
	}

	// Check that the resources correspond to an integer number of compute units, based on what the
	// pod was most recently informed of. The resources may only be mismatched if one of them is at
	// the minimum or maximum of what's allowed for this VM.
	if pod.mostRecentComputeUnit != nil {
		cu := *pod.mostRecentComputeUnit
		dividesCleanly := FromResourceQuantity(req.VCPU)%FromResourceQuantity(cu.VCPU) == 0 && req.Mem%cu.Mem == 0 && uint16(FromResourceQuantity(req.VCPU)/FromResourceQuantity(cu.VCPU)) == req.Mem/cu.Mem
		atMin := milliVCPUs == pod.vCPU.Min || req.Mem == pod.memSlots.Min
		atMax := milliVCPUs == pod.vCPU.Max || req.Mem == pod.memSlots.Max
		if !dividesCleanly && !(atMin || atMax) {
			contextString := "If the VM's bounds did not just change, then this indicates a bug in the autoscaler-agent."
			klog.Warningf(
				"[autoscale-enforcer] requested resources %+v do not divide cleanly by previous compute unit %+v. %s",
				req, cu, contextString,
			)
		}
	}

	vCPUTransition := collectResourceTransition(&node.vCPU, &pod.vCPU)
	memTransition := collectResourceTransition(&node.memSlots, &pod.memSlots)

	vCPUVerdict := vCPUTransition.handleRequested(
		FromResourceQuantity(req.VCPU), startingMigration)
	memVerdict := memTransition.handleRequested(req.Mem, startingMigration)

	fmtString := "[autoscale-enforcer] Handled resources from pod %v AgentRequest.\n" +
		"\tvCPU verdict: %s\n" +
		"\t mem verdict: %s"
	klog.Infof(fmtString, pod.name, vCPUVerdict, memVerdict)

	return api.Resources{VCPU: pod.vCPU.Reserved.toResourceQuantity(), Mem: pod.memSlots.Reserved}, 200, nil
}

func (e *AutoscaleEnforcer) updateMetricsAndCheckMustMigrate(
	pod *podState,
	node *nodeState,
	metrics *api.Metrics,
) bool {
	// This pod should migrate if (a) we're looking for migrations and (b) it's next up in the
	// priority queue. We will give it a chance later to veto if the metrics have changed too much
	//
	// A third condition, "the pod is marked to always migrate" causes it to migrate even if neither
	// of the above conditions are met, so long as it has *previously* provided metrics.
	shouldMigrate := node.mq.isNextInQueue(pod) && node.tooMuchPressure()
	forcedMigrate := pod.testingOnlyAlwaysMigrate && pod.metrics != nil

	klog.Infof("[autoscale-enforcer] Updating pod %v metrics %+v -> %+v", pod.name, pod.metrics, metrics)
	oldMetrics := pod.metrics
	pod.metrics = metrics
	if pod.currentlyMigrating() {
		return false // don't do anything else; it's already migrating.
	}

	node.mq.addOrUpdate(pod)

	if !shouldMigrate && !forcedMigrate {
		return false
	}

	// Give the pod a chance to veto migration if its metrics have significantly changed...
	var veto error
	if oldMetrics != nil && !forcedMigrate {
		veto = pod.checkOkToMigrate(*oldMetrics)
	}

	// ... but override the veto if it's still the best candidate anyways.
	stillFirst := node.mq.isNextInQueue(pod)

	if forcedMigrate || stillFirst || veto == nil {
		if veto != nil {
			klog.Infof("[autoscale-enforcer] Pod attempted veto of self migration, still highest-priority: %s", veto)
		}

		return true
	} else {
		klog.Infof("[autoscale-enforcer] Pod vetoed self migration: %s", veto)
		return false
	}
}
