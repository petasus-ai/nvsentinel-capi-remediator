/*
Copyright 2026 SK Telecom Co., Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package controller polls every workload cluster for NVSentinel signals and
// applies the resulting decisions to the Cluster API Machines behind the
// affected nodes.
package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/capi"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/decision"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal/condition"
)

// ControllerName names the controller in logs, Events and the cluster cache.
const ControllerName = "nvsentinel-capi-remediator"

// DefaultPollInterval is how often each workload cluster's signals are read
// when nothing else is configured. The sources are conditions and objects
// that NVSentinel already debounces with its own check intervals, and the
// reads are served from a per-cluster cache, so the interval stays coarse.
const DefaultPollInterval = 2 * time.Minute

// Event reasons and actions recorded by the controller, next to those in
// package capi.
const (
	// ActionReport is the action of Events about signals that are reported
	// and not acted on.
	ActionReport = "Report"
	// ActionRestart is the action of Events about restart requests.
	ActionRestart = "Restart"

	// EventNodeHealthReported is recorded on the Machine when a signal is
	// reported and nothing is done about it.
	EventNodeHealthReported = "NodeHealthReported"
	// EventRestartUnavailable is recorded on the Machine when a signal
	// calls for a restart, which this controller cannot request yet.
	EventRestartUnavailable = "RestartUnavailable"
	// EventMachineNotFound is recorded on the Cluster when a node with a
	// signal has no Machine, so there is nothing to act on.
	EventMachineNotFound = "MachineNotFound"
)

// ClusterReconciler polls the workload clusters of a management cluster and
// decides, per signal, what to do to the Machine behind the node.
type ClusterReconciler struct {
	// Client reaches the management cluster.
	Client client.Client
	// ClusterCache hands out clients for the workload clusters.
	ClusterCache clustercache.ClusterCache
	// Recorder records Events on Machines and Clusters. Required unless
	// DryRun is set.
	Recorder events.EventRecorder
	// Table maps recommended actions to decisions.
	Table *decision.Table
	// DryRun logs every decision and writes nothing: no annotation and no
	// Event. It is how the decision table is validated against live faults
	// before anything is allowed to delete a node.
	DryRun bool
	// PollInterval overrides DefaultPollInterval when set.
	PollInterval time.Duration
	// ClusterSelector limits the Clusters acted on. Nil selects every
	// Cluster.
	ClusterSelector labels.Selector

	mu sync.Mutex
	// seen holds, per Cluster, what was last concluded about each signal,
	// so that a persisting signal is logged and recorded once rather than
	// on every poll. It is lost on restart, which repeats each Event once.
	seen map[client.ObjectKey]map[string]observation
}

// observation is the conclusion reached about one signal. It deliberately
// leaves out the error codes and GPU UUIDs: the signal is keyed by node and
// check, and a second device failing the same check on the same node is
// the same conclusion, not news.
type observation struct {
	Decision  decision.Decision
	Action    string
	Truncated bool
	// Machine is the Machine behind the node, empty when none owns it.
	Machine string
	// Skip is why a remediation was not carried out, if it was not.
	Skip capi.SkipReason
}

// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile reads every signal of one workload cluster and acts on each.
// It requeues itself every poll interval; the watches only shorten the
// wait after a Cluster changes or connects.
func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	cluster := &clusterv1.Cluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			r.forget(req.NamespacedName)
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	switch {
	case !cluster.DeletionTimestamp.IsZero():
		r.forget(req.NamespacedName)
		return ctrl.Result{}, nil
	case r.ClusterSelector != nil && !r.ClusterSelector.Matches(labels.Set(cluster.Labels)):
		r.forget(req.NamespacedName)
		return ctrl.Result{}, nil
	case annotations.IsPaused(cluster, cluster):
		// Cluster API defers every action on a paused Cluster, and so does
		// this controller. Only spec.paused transitions are watched; the
		// paused annotation is not, so keep polling to notice its removal.
		// A poll of a paused Cluster is one cached Get.
		log.V(1).Info("Cluster is paused, not collecting signals")
		return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
	}

	reader, err := r.ClusterCache.GetReader(ctx, req.NamespacedName)
	if err != nil {
		if errors.Is(err, clustercache.ErrClusterNotConnected) {
			// The Cluster is still provisioning, or its API server is
			// unreachable. The cluster cache enqueues the Cluster again the
			// moment it connects.
			log.V(1).Info("workload cluster is not connected, waiting")
			return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
		}

		return ctrl.Result{}, fmt.Errorf("workload client for %s: %w", req.NamespacedName, err)
	}

	source := condition.NewSource(reader)
	signals, err := source.Collect(ctx)
	if err != nil {
		// Workload API servers come and go during upgrades and rollouts.
		// Poll again rather than failing the reconcile into backoff.
		log.Info("collecting signals failed, will retry", "source", source.Name(), "error", err.Error())
		return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
	}

	machines, err := r.machinesByNode(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	previous := r.observations(req.NamespacedName)
	current := make(map[string]observation, len(signals))
	for _, sig := range signals {
		current[sig.Key()] = r.handle(ctx, cluster, sig, machines[sig.Node], previous[sig.Key()])
	}
	r.remember(ctx, req.NamespacedName, previous, current)

	return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
}

// handle decides one signal, acts on it and returns what was concluded. The
// previous observation decides whether the signal is news: only news is
// logged at the default level and recorded as an Event, so a fault that
// persists across polls does not repeat itself.
func (r *ClusterReconciler) handle(
	ctx context.Context, cluster *clusterv1.Cluster, sig signal.Signal, machine *clusterv1.Machine, previous observation,
) observation {
	outcome := r.Table.Decide(sig)
	obs := observation{Decision: outcome.Decision, Action: outcome.Action, Truncated: sig.Truncated}
	if machine != nil {
		obs.Machine = machine.Name
	}

	log := ctrl.LoggerFrom(ctx).WithValues(
		"node", sig.Node,
		"machine", obs.Machine,
		"source", sig.Origin,
		"check", sig.Check,
		"errorCodes", sig.ErrorCodes,
		"actions", sig.Actions,
		"decision", outcome.Decision,
		"reason", outcome.Reason,
		"dryRun", r.DryRun,
	)
	if len(sig.GpuUUIDs) > 0 {
		log = log.WithValues("gpuUUIDs", sig.GpuUUIDs)
	}
	if sig.Truncated {
		// The source dropped trailing events, so the decision may be too
		// mild. Worth knowing when it looks wrong.
		log = log.WithValues("truncated", true)
	}

	reason := fmt.Sprintf("%s %s: %s", sig.Origin, describe(sig), outcome.Reason)

	switch {
	case machine == nil:
		r.report(log, obs != previous, cluster, EventMachineNotFound, ActionReport,
			fmt.Sprintf("node %s reports %s but no Machine owns it", sig.Node, describe(sig)))
	case outcome.Decision == decision.Replace:
		skip, err := r.actuator().MarkForRemediation(ctx, machine, reason)
		if err != nil {
			log.Error(err, "marking Machine for remediation failed")
			// Nothing was concluded; the next poll starts over.
			return observation{}
		}
		obs.Skip = skip

		switch {
		case skip != "":
			// The actuator drops Events for skips that mean remediation is
			// under way, so recording news here cannot spam.
			if obs != previous {
				log.Info("remediation skipped", "skip", skip, "why", skip.Message())
				r.actuator().RecordSkipped(machine, skip, reason)
			} else {
				log.V(1).Info("remediation still skipped", "skip", skip)
			}
		case r.DryRun:
			r.report(log, obs != previous, nil, "", "", "would mark Machine for remediation")
		default:
			// The actuator recorded the Event; the annotation guards make
			// this happen once.
			log.Info("Machine marked for remediation")
		}
	case outcome.Decision == decision.Restart:
		r.report(log, obs != previous, machine, EventRestartUnavailable, ActionRestart,
			fmt.Sprintf("%s calls for a restart of node %s, which is not available yet; reported only",
				describe(sig), sig.Node))
	default:
		r.report(log, obs != previous, machine, EventNodeHealthReported, ActionReport,
			fmt.Sprintf("%s on node %s: %s", describe(sig), sig.Node, outcome.Reason))
	}

	return obs
}

// report logs a conclusion that involves no action and, when it is news and
// this is not a dry run, records it as a Warning Event on object. A nil
// object records nothing.
func (r *ClusterReconciler) report(log logr.Logger, news bool, object client.Object, eventReason, action, message string) {
	if !news {
		log.V(1).Info("health signal unchanged")
		return
	}

	log.Info(message)

	if r.DryRun || object == nil {
		return
	}
	r.Recorder.Eventf(object, nil, corev1.EventTypeWarning, eventReason, action, "%s", capi.EventNote(message))
}

// actuator applies decisions to Machines with this controller's settings.
func (r *ClusterReconciler) actuator() *capi.Actuator {
	return &capi.Actuator{Client: r.Client, Recorder: r.Recorder, DryRun: r.DryRun}
}

// machinesByNode indexes the Cluster's Machines by the node they own, which
// is the only reliable link from a workload cluster node name back to a
// management cluster object.
func (r *ClusterReconciler) machinesByNode(ctx context.Context, cluster *clusterv1.Cluster) (map[string]*clusterv1.Machine, error) {
	machines := &clusterv1.MachineList{}
	if err := r.Client.List(ctx, machines,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name},
	); err != nil {
		return nil, fmt.Errorf("list machines of %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}

	byNode := make(map[string]*clusterv1.Machine, len(machines.Items))
	for i := range machines.Items {
		if name := machines.Items[i].Status.NodeRef.Name; name != "" {
			byNode[name] = &machines.Items[i]
		}
	}

	return byNode, nil
}

// describe summarises a signal for an operator, e.g.
// "GpuXidError DCGM_FR_XID_ERROR gpu GPU-1234 (REPLACE_VM)".
func describe(s signal.Signal) string {
	b := s.Check
	if len(s.ErrorCodes) > 0 {
		b += " " + strings.Join(s.ErrorCodes, ",")
	}
	if len(s.GpuUUIDs) > 0 {
		b += " gpu " + strings.Join(s.GpuUUIDs, ",")
	}
	if len(s.Actions) > 0 {
		b += " (" + strings.Join(s.Actions, ",") + ")"
	}
	if s.Truncated {
		b += " [truncated]"
	}

	return b
}

func (r *ClusterReconciler) pollInterval() time.Duration {
	if r.PollInterval > 0 {
		return r.PollInterval
	}

	return DefaultPollInterval
}

// observations returns a copy of what was last concluded for the Cluster.
func (r *ClusterReconciler) observations(key client.ObjectKey) map[string]observation {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make(map[string]observation, len(r.seen[key]))
	for k, v := range r.seen[key] {
		out[k] = v
	}

	return out
}

// remember stores the Cluster's current observations and logs the signals
// that cleared since the last poll.
func (r *ClusterReconciler) remember(ctx context.Context, key client.ObjectKey, previous, current map[string]observation) {
	log := ctrl.LoggerFrom(ctx)
	for k, prev := range previous {
		if _, still := current[k]; !still {
			log.Info("health signal cleared", "signal", k, "machine", prev.Machine, "decision", prev.Decision)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.seen == nil {
		r.seen = map[client.ObjectKey]map[string]observation{}
	}
	if len(current) == 0 {
		delete(r.seen, key)
		return
	}
	r.seen[key] = current
}

// forget drops everything remembered about a Cluster that is gone, being
// deleted or no longer selected.
func (r *ClusterReconciler) forget(key client.ObjectKey) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.seen, key)
}

// SetupWithManager registers the controller. It reconciles on Cluster
// creation, spec changes and paused transitions, and whenever the cluster
// cache connects to or loses a workload cluster; otherwise it polls. The
// selector filter does not reach the cluster cache's source, so Reconcile
// checks the selector again; the manager's own cache is restricted to the
// selector too, which keeps unselected Clusters out of both.
func (r *ClusterReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	predicateLog := ctrl.LoggerFrom(ctx).WithValues("controller", ControllerName)

	b := ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		For(&clusterv1.Cluster{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicates.ClusterPausedTransitions(mgr.GetScheme(), predicateLog),
		))).
		WatchesRawSource(r.ClusterCache.GetClusterSource(ControllerName, clusterToRequest)).
		WithOptions(options)

	if r.ClusterSelector != nil {
		b = b.WithEventFilter(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return r.ClusterSelector.Matches(labels.Set(o.GetLabels()))
		}))
	}

	return b.Complete(r)
}

// clusterToRequest maps a Cluster event from the cluster cache to its own
// reconcile request.
func clusterToRequest(_ context.Context, o client.Object) []ctrl.Request {
	return []ctrl.Request{{NamespacedName: client.ObjectKeyFromObject(o)}}
}
