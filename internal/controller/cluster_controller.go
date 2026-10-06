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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/capi"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/decision"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal/extrr"
)

// ControllerName names the controller in logs, Events and the cluster cache.
const ControllerName = "nvsentinel-capi-remediator"

// DefaultPollInterval is how often each workload cluster's signals are read
// when nothing else is configured. The sources are conditions and objects
// that NVSentinel already debounces with its own check intervals, and the
// reads are served from a per-cluster cache, so the interval stays coarse.
// A request does not wait for it to be read (see watchRequests);
// everything else does, including noticing that a restart has completed.
const DefaultPollInterval = 2 * time.Minute

// earlierMarkGrace is how long a restart is held back for the
// MachineHealthChecks to drop what they hold for the mark the Machine
// carried before (see capi.EarlierMarkPending). Cluster API runs a check at
// most once in fifteen seconds, so a check that works has had its turn well
// within it. One that has not rewritten the condition by then most likely
// has nothing to drop, and waiting on would keep a faulty node out of
// service for a sign that may never change.
const earlierMarkGrace = time.Minute

// requestWatchName names the watch on a workload cluster's
// ExternalRemediationRequests in the cluster cache.
const requestWatchName = ControllerName + "-watchRequests"

// Event reasons and actions recorded by the controller, next to those in
// package capi.
const (
	// ActionReport is the action of Events about signals that are reported
	// and not acted on.
	ActionReport = "Report"
	// ActionSelectSource is the action of Events about choosing where a
	// cluster's signals are read from.
	ActionSelectSource = "SelectSource"

	// EventSignalSourceSelected is recorded on the Cluster when the mode it
	// is read in is first chosen or changes.
	EventSignalSourceSelected = "SignalSourceSelected"

	// ActionAnswer is the action of Events about answering an
	// ExternalRemediationRequest.
	ActionAnswer = "Answer"
	// EventRequestAnswered is recorded on the Machine, or on the Cluster
	// when the node has none, once an ExternalRemediationRequest is
	// answered.
	EventRequestAnswered = "ExternalRemediationRequestAnswered"

	// EventNodeHealthReported is recorded on the Machine when a signal is
	// reported and nothing is done about it.
	EventNodeHealthReported = "NodeHealthReported"
	// EventReplaceUnavailable is recorded on the Machine when a signal calls
	// for a replacement but no MachineHealthCheck selects the Machine, or
	// only paused ones, so marking it would do nothing for now.
	EventReplaceUnavailable = "ReplaceUnavailable"
	// EventRestartUnavailable is recorded on the Machine when a signal
	// calls for a restart but not every MachineHealthCheck selecting the
	// Machine remediates through a template, so marking it would replace
	// the Machine or do nothing.
	EventRestartUnavailable = "RestartUnavailable"
	// EventMachineNotFound is recorded on the Cluster when a node with a
	// signal has no Machine, so there is nothing to act on.
	EventMachineNotFound = "MachineNotFound"
)

// Reasons given with an answer to an ExternalRemediationRequest, next to the
// capi.SkipReason values for Machines this operator will not touch.
const (
	// AnswerRestarted: the node restarted and is Ready again.
	AnswerRestarted = "Restarted"
	// AnswerNotRemediated: the decision table reports the recommended
	// action rather than acting on it.
	AnswerNotRemediated = "NotRemediated"
	// AnswerMachineNotFound: no Machine owns the node.
	AnswerMachineNotFound = "MachineNotFound"
	// AnswerRemediationUnavailable: no MachineHealthCheck can carry out
	// what the decision asks for.
	AnswerRemediationUnavailable = "RemediationUnavailable"
)

// RestartFallback is what happens to a restart that no remediation template
// can carry out.
type RestartFallback string

const (
	// RestartFallbackReport reports the signal with an Event and leaves the
	// Machine alone. It is the default: a restart is the cheaper repair,
	// and replacing a node for it is a choice to make deliberately.
	RestartFallbackReport RestartFallback = "report"
	// RestartFallbackReplace hands the Machine to Cluster API for a
	// replacement instead, for operators who prefer an automatic recovery
	// over a repair their provider cannot offer.
	RestartFallbackReplace RestartFallback = "replace"
)

// RestartFallbackAnnotation on a Cluster overrides the operator's restart
// fallback for that Cluster, with the value report or replace.
const RestartFallbackAnnotation = capi.AnnotationPrefix + "/restart-fallback"

// ParseRestartFallback validates a restart fallback given as text.
func ParseRestartFallback(s string) (RestartFallback, error) {
	switch f := RestartFallback(s); f {
	case RestartFallbackReport, RestartFallbackReplace:
		return f, nil
	default:
		return "", fmt.Errorf("restart fallback %q is neither %q nor %q", s, RestartFallbackReport, RestartFallbackReplace)
	}
}

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
	// RestartFallback is what happens to a restart that no remediation
	// template can carry out, unless the Cluster overrides it with
	// RestartFallbackAnnotation. Empty means RestartFallbackReport.
	RestartFallback RestartFallback

	mu sync.Mutex
	// seen holds, per Cluster, what was last concluded about each signal,
	// so that a persisting signal is logged and recorded once rather than
	// on every poll. It is lost on restart, which repeats each Event once.
	seen map[client.ObjectKey]map[string]observation
	// wouldRelease holds, per Cluster, the Machines a dry run found ready
	// to be released, which it never releases, so that saying so happens
	// once rather than on every poll.
	wouldRelease map[client.ObjectKey]map[string]bool
	// modes holds the mode each Cluster was last read in, so that a change
	// is logged and recorded once.
	modes map[client.ObjectKey]Mode
	// waiting holds, per Cluster, the Machines a restart was held back for
	// (see heldForChecks). It is lost on restart, which starts the waits
	// over.
	waiting map[client.ObjectKey]map[string]*checksWait
	// now is the clock; time.Now when nil.
	now func() time.Time

	// requestWatcher is the controller that the watches on the workload
	// clusters' requests are added to. SetupWithManager sets it.
	requestWatcher clustercache.SourceWatcher[ctrl.Request]
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
	// Request names the ExternalRemediationRequest the signal came from. A
	// new request for the same node and check is a new fault.
	Request string
	// Skip is why a remediation was not carried out, if it was not.
	Skip capi.SkipReason
	// Coverage describes the MachineHealthChecks selecting the Machine,
	// when the decision called for them. A change in them changes what
	// marking the Machine does.
	Coverage string
	// Fallback is the restart fallback applied, when a restart could not
	// be carried out, with a note when the Cluster's override was invalid.
	Fallback string
	// Answer is what the ExternalRemediationRequest the signal came from
	// is to be answered, if anything yet.
	Answer answer
}

// answer is the outcome reported back to an ExternalRemediationRequest.
// The zero value means there is nothing to report yet.
type answer struct {
	// Complete is true once the node is remediated, false when it will not
	// be.
	Complete bool
	Reason   string
	Message  string
}

// given reports whether there is an answer to report.
func (a answer) given() bool {
	return a.Reason != ""
}

// decline is the answer for a signal this operator will not act on: False
// for a signal read from a request, nothing for any other.
func decline(sig signal.Signal, reason, message string) answer {
	if sig.Request == "" {
		return answer{}
	}

	return answer{Complete: false, Reason: reason, Message: message}
}

// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinehealthchecks,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile reads every signal of one workload cluster and acts on each.
// It requeues itself every poll interval, or sooner while a restart is held
// back for the Machine's checks; the watches only shorten the wait after a
// Cluster changes or connects, or NVSentinel releases a node to a request.
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
		// Whatever paused the Cluster may have paused its checks too, as
		// spec.paused does, so time spent paused is not counted as time
		// they had.
		r.forgetWaits(req.NamespacedName, nil)
		return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
	}

	workload, err := r.ClusterCache.GetClient(ctx, req.NamespacedName)
	if err != nil {
		if errors.Is(err, clustercache.ErrClusterNotConnected) {
			// The Cluster is still provisioning, or its API server is
			// unreachable. The cluster cache enqueues the Cluster again the
			// moment it connects.
			log.V(1).Info("workload cluster is not connected, waiting")
			// The checks may not reach the workload cluster either, in
			// which case they remediate nothing, so this time is not
			// counted as time they had.
			r.forgetWaits(req.NamespacedName, nil)
			return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
		}

		return ctrl.Result{}, fmt.Errorf("workload client for %s: %w", req.NamespacedName, err)
	}

	mode, err := selectMode(workload.RESTMapper())
	if err != nil {
		// Discovery fails like any other read of an API server that is
		// going through an upgrade. Poll again.
		log.Info("selecting the signal source failed, will retry", "error", err.Error())
		return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
	}
	r.noteMode(log, cluster, mode)

	if mode == ModeExternalRemediationRequest {
		if err := r.watchRequests(ctx, req.NamespacedName); err != nil {
			// The poll reads the requests whether or not they are watched.
			log.Info("watching the requests failed, they are read at every poll only", "error", err.Error())
		}
	}

	source := mode.source(workload)
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

	var checks []clusterv1.MachineHealthCheck
	if len(signals) > 0 {
		if checks, err = capi.ListHealthChecks(ctx, r.Client, cluster); err != nil {
			return ctrl.Result{}, err
		}
	}

	nodes := map[string]*corev1.Node{}
	if len(signals) > 0 {
		if nodes, err = nodesByName(ctx, workload); err != nil {
			log.Info("reading nodes failed, will retry", "error", err.Error())
			return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
		}
	}

	// Requests are acted on and answered through an uncached client: the
	// cached view of the requests can lag behind that of the Machines, and
	// a request answered a moment ago must not have its Machine marked
	// again.
	var live client.Client
	if mode == ModeExternalRemediationRequest && len(signals) > 0 {
		if live, err = r.ClusterCache.GetUncachedClient(ctx, req.NamespacedName); err != nil {
			log.Info("reaching the workload cluster failed, will retry", "error", err.Error())
			return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
		}
	}

	previous := r.observations(req.NamespacedName)
	current := make(map[string]observation, len(signals))
	// asking are the requests found pending in this pass. The observations
	// cannot stand in for them: they are kept by node and check, so one of
	// two requests for the same check of a node replaces the other.
	asking := map[string]bool{}
	unreadable := false
	for _, sig := range signals {
		if sig.Request != "" {
			pending, err := extrr.IsPending(ctx, live, sig.Request)
			if err != nil {
				log.Info("reading the request failed, will retry", "request", sig.Request, "error", err.Error())
				unreadable = true
				if prev, ok := previous[sig.Key()]; ok {
					current[sig.Key()] = prev
				}
				continue
			}
			if !pending {
				continue
			}
			asking[sig.Request] = true
		}

		machine, node := machines[sig.Node], nodes[sig.Node]
		obs := r.handle(ctx, cluster, mode, checks, sig, machine, node, previous[sig.Key()])
		if obs.Answer.given() {
			r.answer(ctx, live, cluster, machine, sig, obs.Answer, obs != previous[sig.Key()])
		}
		current[sig.Key()] = obs
	}
	// A request that could not be read may still hold its Machine, and
	// nothing may be released on the strength of a partial read.
	if !unreadable {
		r.releaseRestarts(ctx, req.NamespacedName, source.Name(), machines, nodes, current, asking)
	}
	r.remember(ctx, req.NamespacedName, previous, current)
	r.forgetWaits(req.NamespacedName, machines)

	return ctrl.Result{RequeueAfter: r.nextLook(req.NamespacedName)}, nil
}

// handle decides one signal, acts on it and returns what was concluded. The
// previous observation decides whether the signal is news: only news is
// logged at the default level and recorded as an Event, so a fault that
// persists across polls does not repeat itself. In ModeReportOnly every
// decision is reported, whatever the table says. For a signal read from an
// ExternalRemediationRequest the observation also carries the answer to
// give, once there is one.
func (r *ClusterReconciler) handle(
	ctx context.Context, cluster *clusterv1.Cluster, mode Mode, checks []clusterv1.MachineHealthCheck,
	sig signal.Signal, machine *clusterv1.Machine, node *corev1.Node, previous observation,
) observation {
	outcome := r.Table.Decide(sig)
	if why := reportOnly(mode, outcome.Decision); why != "" {
		outcome.Reason += "; reported only since " + why
		outcome.Decision = decision.Report
	}
	obs := observation{Decision: outcome.Decision, Action: outcome.Action, Truncated: sig.Truncated, Request: sig.Request}
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

	switch {
	case node == nil && sig.Request != "":
		// The node is gone, most likely replaced. NVSentinel makes the node
		// the request's owner, so Kubernetes deletes the request with it:
		// there is nobody left to answer.
		log.V(1).Info("the request's node no longer exists, leaving the request to be deleted with it")
	case machine == nil:
		message := fmt.Sprintf("node %s reports %s but no Machine owns it", sig.Node, describe(sig))
		obs.Answer = decline(sig, AnswerMachineNotFound, message)
		r.report(log, obs != previous, cluster, EventMachineNotFound, ActionReport, message)
	case outcome.Decision == decision.Replace, outcome.Decision == decision.Restart:
		return r.remediate(ctx, log, cluster, checks, sig, outcome, machine, node, obs, previous)
	default:
		message := fmt.Sprintf("%s on node %s: %s", describe(sig), sig.Node, outcome.Reason)
		obs.Answer = decline(sig, AnswerNotRemediated, message)
		r.report(log, obs != previous, machine, EventNodeHealthReported, ActionReport, message)
	}

	return obs
}

// remediate hands the Machine to Cluster API when the MachineHealthChecks
// selecting it will do what the decision asks: any remediation at all for
// a Replace, and only remediation templates for a Restart, since a check
// without a template would replace the Machine instead. A Restart that no
// template can carry out follows the restart fallback: reported, or
// handed over as a Replace. Otherwise the signal is reported. Guards come
// first, so that a Machine already being remediated reads as in progress
// rather than unavailable.
func (r *ClusterReconciler) remediate(
	ctx context.Context, log logr.Logger, cluster *clusterv1.Cluster, checks []clusterv1.MachineHealthCheck,
	sig signal.Signal, outcome decision.Outcome, machine *clusterv1.Machine, node *corev1.Node, obs, previous observation,
) observation {
	restart := outcome.Decision == decision.Restart
	action, verb := capi.ActionRemediate, "a replacement"
	if restart {
		action, verb = capi.ActionRestart, "a restart"
	}

	// NVSentinel stops watching a node it has released to a request, so
	// the signal never clears by itself: a restart asked for through a
	// request is complete once the node has restarted and is back. That is
	// judged before anything else, so that a change of the checks during
	// the restart does not turn a finished restart into a replacement. The
	// Machine is released only once the request is answered (see answer),
	// so that a failed answer never leaves a pending request behind an
	// unmarked Machine, which the next poll would restart again.
	//
	// A restart answers the request it was asked for and no other. A
	// request that finds the mark of a restart that is over is treated as
	// raised for a fault the restart did not cure, and answering it too
	// would report a remediation that never happened: that mark is what a
	// failed release left behind, or a restart of the operator between the
	// answer and the release. Such a request is handled like any other
	// that finds its Machine marked. Where it calls for a restart it waits,
	// releaseRestarts removes the mark, and a later pass asks for a restart
	// of its own; where the restart falls back to a replacement, the mark
	// is turned into one below.
	if restart && sig.Request != "" && capi.RestartCompleted(machine, node) && capi.RestartAnswers(machine, sig.Request) {
		obs.Answer = answer{Complete: true, Reason: AnswerRestarted,
			Message: fmt.Sprintf("node %s restarted and is Ready", sig.Node)}
		if obs != previous {
			log.Info("restart completed")
		}

		return obs
	}

	// The checks decide what marking does, and so whether a restart can be
	// asked for at all or falls back.
	cov := capi.CoverageOf(checks, machine)
	// It stays set when the replace fallback takes over, so that the
	// fallback applied is recorded on both paths.
	restartUnavailable := restart && !cov.TemplatesOnly()
	var (
		fallback     RestartFallback
		fallbackNote string
	)
	if restartUnavailable {
		fallback, fallbackNote = r.restartFallback(cluster)
		// Replacing needs a MachineHealthCheck too, so without one the
		// replace fallback has nothing to hand the Machine to either.
		if fallback == RestartFallbackReplace && cov.Covered() {
			restart = false
			action, verb = capi.ActionRemediate, "a replacement in place of a restart"
			outcome.Reason += ", which no remediation template can carry out; falling back to a replacement" + fallbackNote
		}
	}

	if !restart {
		// A replacement called for after this operator asked for a restart
		// must outlive the restart: the mark is turned into a replacement
		// mark, which is never released. A dry run cannot change the mark,
		// so it may later say it would release a Machine a live run keeps.
		escalated, err := r.actuator().Escalate(ctx, machine,
			fmt.Sprintf("%s %s: %s", sig.Origin, describe(sig), outcome.Reason))
		switch {
		case err != nil:
			log.Error(err, "escalating the restart to a replacement failed")
			// Nothing was concluded; the next poll starts over.
			return observation{}
		case escalated && !r.DryRun:
			log.Info("restart escalated to a replacement")
		}
	}

	if skip := capi.Guard(machine); skip != "" {
		obs.Skip = skip
		if !skip.InProgress() && skip != capi.SkipPaused {
			// Nobody is going to act on the Machine; a paused one is only
			// waiting.
			obs.Answer = decline(sig, string(skip), fmt.Sprintf("%s on node %s: remediation skipped because %s",
				describe(sig), sig.Node, skip.Message()))
		}
		// The actuator drops Events for skips that mean remediation is
		// under way, so recording news here cannot spam.
		if obs != previous {
			log.Info("remediation skipped", "skip", skip, "why", skip.Message())
			r.actuator().RecordSkipped(machine, skip, action, fmt.Sprintf("%s %s: %s", sig.Origin, describe(sig), outcome.Reason))
		} else {
			log.V(1).Info("remediation still skipped", "skip", skip)
		}

		return obs
	}

	obs.Coverage = cov.Describe()
	log = log.WithValues("coverage", obs.Coverage)
	if restartUnavailable {
		obs.Fallback = string(fallback) + fallbackNote
		log = log.WithValues("restartFallback", obs.Fallback)
	}

	// A paused check may carry out the decision once it is unpaused, so a
	// request waits for that instead of being declined for good.
	unpaused := cov.IgnoringPause()
	switch {
	case restart && restartUnavailable:
		waiting := unpaused.TemplatesOnly() || (fallback == RestartFallbackReplace && unpaused.Covered())
		message := fmt.Sprintf("%s calls for a restart of node %s, but %s; %s (restart fallback %s)",
			describe(sig), sig.Node, obs.Coverage, unavailable(waiting), obs.Fallback)
		if !waiting {
			obs.Answer = decline(sig, AnswerRemediationUnavailable, message)
		}
		r.report(log, obs != previous, machine, EventRestartUnavailable, capi.ActionRestart, message)
		return obs
	case !restart && !cov.Covered():
		waiting := unpaused.Covered()
		message := fmt.Sprintf("%s calls for a replacement of node %s, but %s; %s",
			describe(sig), sig.Node, obs.Coverage, unavailable(waiting))
		if !waiting {
			obs.Answer = decline(sig, AnswerRemediationUnavailable, message)
		}
		r.report(log, obs != previous, machine, EventReplaceUnavailable, capi.ActionRemediate, message)
		return obs
	}

	// A restart is held back while the checks still show the mark the
	// Machine carried before: they would take the new mark for the old one,
	// and the provider would count this restart against the earlier one. A
	// replacement does not wait, since it is what the provider arrives at
	// either way.
	if restart {
		switch held, shown := r.heldForChecks(client.ObjectKeyFromObject(cluster), machine); {
		case held:
			obs.Skip = capi.SkipEarlierMark
			if obs != previous {
				log.Info("remediation skipped", "skip", obs.Skip, "why", obs.Skip.Message())
			} else {
				log.V(1).Info("remediation still skipped", "skip", obs.Skip)
			}

			return obs
		case shown > 0:
			log.Info("the checks still show the mark the Machine carried before, not holding the restart back",
				"shownFor", shown.String())
		}
	}

	// A Replace on a Machine whose checks all remediate through templates
	// gets the provider's remediation first and a replacement only once
	// that gives up; with any check remediating by replacement it is
	// replaced at once. The reason names the checks, since they decide.
	reason := fmt.Sprintf("%s %s: %s; %s", sig.Origin, describe(sig), outcome.Reason, obs.Coverage)

	if r.DryRun {
		r.report(log, obs != previous, nil, "", "", "would mark Machine for remediation, asking for "+verb+fallbackNote)
		return obs
	}

	// The node's boot ID is recorded with a restart, which is how its
	// completion is told when the request it came from never clears, and
	// so is that request, which is the one the restart answers.
	record := capi.RestartRecord{}
	if restart {
		record.Request = sig.Request
		if node != nil {
			record.BootID = node.Status.NodeInfo.BootID
		}
	}
	// The actuator runs the same guards on the same object, so it cannot
	// skip what passed them above.
	if _, err := r.actuator().MarkForRemediation(ctx, machine, action, reason, record); err != nil {
		log.Error(err, "marking Machine for remediation failed")
		// Nothing was concluded; the next poll starts over.
		return observation{}
	}

	// The actuator recorded the Event; the annotation guards make this
	// happen once.
	log.Info("Machine marked for remediation", "asking", verb)

	return obs
}

// restartFallback returns the restart fallback for the Cluster: its
// annotation when that holds a valid value, the operator's setting
// otherwise. The note explains an annotation that was ignored, so that the
// mistake shows in the Events of the signals it affects.
func (r *ClusterReconciler) restartFallback(cluster *clusterv1.Cluster) (RestartFallback, string) {
	fallback := r.RestartFallback
	if fallback == "" {
		fallback = RestartFallbackReport
	}

	value, ok := cluster.Annotations[RestartFallbackAnnotation]
	if !ok {
		return fallback, ""
	}
	override, err := ParseRestartFallback(value)
	if err != nil {
		return fallback, fmt.Sprintf("; the Cluster's %s annotation is ignored: %v", RestartFallbackAnnotation, err)
	}

	return override, ""
}

// releaseRestarts releases every Machine this operator marked for a
// restart once no signal this operator acts on calls for a restart or a
// replacement of its node any more. NVSentinel lowers a condition when its check passes again:
// monitors reading the kernel log clear their conditions once they see a
// new boot, and GPU checks on their first passing run. After a restart
// that is the sign the restart worked; a condition lowered before the
// restart happened means the fault is gone, and releasing then cancels a
// restart nobody needs. Until the mark is removed Cluster API keeps the
// Machine unhealthy, so the provider retries and the Machine is
// eventually replaced. Machines marked for a replacement stay marked
// until Cluster API replaces them.
//
// A request holds the mark of the restart it asked for, and that of a
// restart still under way, whoever asked for it. It does not hold the mark
// of a restart that is over and was asked for by another request, unless
// that request is itself still asking: such a restart cannot answer it (see
// remediate), so the mark is released and the request gets a restart of its
// own at a later pass, once the MachineHealthChecks have had the time to
// drop what they hold for the earlier one (see heldForChecks).
//
// It runs only after the signals were collected successfully, so a
// workload cluster that cannot be read never releases anything. A node
// that disappears from the workload cluster takes its signals with it and
// its Machine is released; the MachineHealthChecks treat a missing node as
// unhealthy on their own. Only Machines with a node are considered, which
// every Machine this operator marks has, since it marks them through their
// node.
func (r *ClusterReconciler) releaseRestarts(
	ctx context.Context, key client.ObjectKey, source string,
	machines map[string]*clusterv1.Machine, nodes map[string]*corev1.Node,
	current map[string]observation, asking map[string]bool,
) {
	nodeOf := make(map[string]string, len(machines))
	for node, m := range machines {
		nodeOf[m.Name] = node
	}
	// held are the Machines a signal holds. leftover are those that carry
	// the mark of a finished restart nobody asks for any more, with the
	// request that waits for a restart of its own.
	held, leftover := map[string]bool{}, map[string]string{}
	for _, obs := range current {
		if obs.Machine == "" || (obs.Decision != decision.Replace && obs.Decision != decision.Restart) {
			continue
		}
		node := nodeOf[obs.Machine]
		if m := machines[node]; m != nil && obs.Decision == decision.Restart && obs.Request != "" {
			// A request that is still asking is the one the mark answers, or
			// is yet to be answered by it; either way the mark stays.
			asked := m.Annotations[capi.RemediationRequestAnnotation]
			if asked != "" && !asking[asked] && capi.RestartCompleted(m, nodes[node]) {
				// The first by name, should several wait, so that the
				// record of the release reads the same every time.
				if waiting, ok := leftover[obs.Machine]; !ok || obs.Request < waiting {
					leftover[obs.Machine] = obs.Request
				}
				continue
			}
		}
		held[obs.Machine] = true
	}

	wouldRelease := map[string]bool{}
	defer r.rememberWouldRelease(key, wouldRelease)

	names := make([]string, 0, len(machines))
	for node := range machines {
		names = append(names, node)
	}
	slices.Sort(names)

	for _, node := range names {
		m := machines[node]
		if action, ok := capi.MarkedAction(m); !ok || action != capi.ActionRestart || held[m.Name] {
			continue
		}

		log := ctrl.LoggerFrom(ctx).WithValues("node", node, "machine", m.Name, "dryRun", r.DryRun)
		reason := fmt.Sprintf("no %s signal this operator acts on calls for a restart or a replacement of node %s any more", source, node)
		why := "its restart signal cleared"
		if waiting, ok := leftover[m.Name]; ok {
			asked := m.Annotations[capi.RemediationRequestAnnotation]
			reason = fmt.Sprintf("the restart of node %s that %s asked for is over, and %s waits for one of its own", node, asked, waiting)
			why = "the restart it was marked for is over"
			log = log.WithValues("askedBy", asked, "request", waiting)
		}
		released, err := r.actuator().Release(ctx, m, reason)
		switch {
		case err != nil:
			// The next poll tries again.
			log.Error(err, "releasing Machine from remediation failed")
		case released && r.DryRun:
			wouldRelease[m.Name] = true
			if r.releasableBefore(key, m.Name) {
				log.V(1).Info("would still release Machine from remediation")
			} else {
				log.Info("would release Machine from remediation, " + why)
			}
		case released:
			log.Info("Machine released from remediation, " + why)
		}
	}
}

// unavailable ends the message about a decision no MachineHealthCheck can
// carry out: it is reported only, or, when unpausing checks would make it
// possible, waits for that.
func unavailable(waiting bool) string {
	if waiting {
		return "waiting for the paused checks"
	}

	return "reported only"
}

// reportOnly returns why a decision taken in the mode is only reported, or
// the empty string when it may be acted on.
func reportOnly(mode Mode, d decision.Decision) string {
	if mode == ModeReportOnly && d != decision.Report {
		return "NVSentinel's janitor remediates this cluster itself"
	}

	return ""
}

// answer reports the outcome of a signal back to the ExternalRemediationRequest
// it came from, and once a completed restart is reported releases the
// Machine from remediation. It logs and records an Event only when the
// answer is written, which happens once: an answered request is no longer
// pending. A failure is logged and the next poll answers again; a release
// that fails after the answer is left to releaseRestarts, since the request
// no longer holds the Machine, and until then the mark answers no other
// request (see remediate). In dry run it only says, once, what it would
// answer.
func (r *ClusterReconciler) answer(
	ctx context.Context, live client.Client, cluster *clusterv1.Cluster, machine *clusterv1.Machine,
	sig signal.Signal, a answer, news bool,
) {
	log := ctrl.LoggerFrom(ctx).WithValues("node", sig.Node, "request", sig.Request,
		"complete", a.Complete, "answerReason", a.Reason, "dryRun", r.DryRun)

	if r.DryRun {
		if news {
			log.Info("would answer ExternalRemediationRequest", "answerMessage", a.Message)
		}
		return
	}

	answered, err := extrr.Answer(ctx, live, sig.Request, a.Complete, a.Reason, a.Message)
	switch {
	case err != nil:
		log.Error(err, "answering ExternalRemediationRequest failed")
		return
	case !answered:
		return
	}

	log.Info("ExternalRemediationRequest answered", "answerMessage", a.Message)
	var object client.Object = cluster
	if machine != nil {
		object = machine
	}
	status := "False"
	if a.Complete {
		status = "True"
	}
	r.Recorder.Eventf(object, nil, corev1.EventTypeNormal, EventRequestAnswered, ActionAnswer, "%s",
		capi.EventNote(fmt.Sprintf("Answered %s %s: %s, %s: %s", extrr.GroupVersionKind.Kind, sig.Request, status, a.Reason, a.Message)))

	if a.Complete && machine != nil {
		if err := r.releaseAnswered(ctx, machine, a.Message); err != nil {
			log.Error(err, "releasing Machine from remediation failed")
		}
	}
}

// releaseAnswered releases the Machine whose restart was just answered.
// The Machine was read before the answer was written, and Cluster API
// writes to it at just that time, as its node comes back. A release that
// loses to such a write is tried again, a few times over a third of a
// second, on the Machine as the manager's cache has it by then, as long as
// that still carries the mark that was answered: a mark that has changed
// since, or is gone, is someone else's doing and is left alone. The Machine
// is brought up to date either way, for the signals handled after this one.
func (r *ClusterReconciler) releaseAnswered(ctx context.Context, machine *clusterv1.Machine, reason string) error {
	answered := restartMarkOf(machine)
	conflicted := false

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// Read after the wait, which is what gives the cache time to catch
		// up with the write that won.
		if conflicted {
			current := &clusterv1.Machine{}
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(machine), current); err != nil {
				return client.IgnoreNotFound(err)
			}
			*machine = *current
			if restartMarkOf(machine) != answered {
				return nil
			}
		}

		_, err := r.actuator().Release(ctx, machine, reason)
		conflicted = apierrors.IsConflict(err)

		// A Machine that is gone has nothing left to release.
		return client.IgnoreNotFound(err)
	})
}

// restartMark is what identifies the restart a Machine is marked for.
type restartMark struct {
	action, bootID, request string
	marked                  bool
}

func restartMarkOf(m *clusterv1.Machine) restartMark {
	action, marked := capi.MarkedAction(m)

	return restartMark{
		action:  action,
		bootID:  m.Annotations[capi.RemediationBootIDAnnotation],
		request: m.Annotations[capi.RemediationRequestAnnotation],
		marked:  marked,
	}
}

// nodesByName indexes the workload cluster's nodes by name.
func nodesByName(ctx context.Context, reader client.Reader) (map[string]*corev1.Node, error) {
	list := &corev1.NodeList{}
	if err := reader.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	byName := make(map[string]*corev1.Node, len(list.Items))
	for i := range list.Items {
		byName[list.Items[i].Name] = &list.Items[i]
	}

	return byName, nil
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
	delete(r.wouldRelease, key)
	delete(r.modes, key)
	delete(r.waiting, key)
}

// checksWait is the wait of one Machine for its checks.
type checksWait struct {
	// since is when the Machine was first seen unmarked with its checks
	// still showing the earlier mark.
	since time.Time
	// over is set once the wait has been ended by its bound, which is said
	// once.
	over bool
}

// heldForChecks reports whether a restart of the Machine is to be held back
// for its checks: they still show the mark it carried before, and have had
// less than earlierMarkGrace to drop what they hold for it since that was
// first seen here. The wait is bounded because the sign it goes by can stay
// for good (see capi.EarlierMarkPending).
//
// The wait belongs to the Machine, not to the signal that ran into it: it
// lasts for as long as the Machine stays that way (see forgetWaits), so
// that a mark that fails, a dry run or the next signal does not start it
// again. shown is how long the Machine has been seen that way, on the first
// pass on which the bound lets a restart through, and zero otherwise.
func (r *ClusterReconciler) heldForChecks(key client.ObjectKey, machine *clusterv1.Machine) (held bool, shown time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !capi.EarlierMarkPending(machine) {
		delete(r.waiting[key], machine.Name)
		return false, 0
	}

	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	wait := r.waiting[key][machine.Name]
	if wait == nil {
		if r.waiting == nil {
			r.waiting = map[client.ObjectKey]map[string]*checksWait{}
		}
		if r.waiting[key] == nil {
			r.waiting[key] = map[string]*checksWait{}
		}
		r.waiting[key][machine.Name] = &checksWait{since: now}

		return true, 0
	}
	switch {
	case now.Sub(wait.since) < earlierMarkGrace:
		return true, 0
	case wait.over:
		return false, 0
	default:
		wait.over = true
		return false, now.Sub(wait.since)
	}
}

// nextLook is when the Cluster is to be reconciled again: at the next poll,
// or when the first wait of one of its Machines is over, should that be
// sooner.
func (r *ClusterReconciler) nextLook(key client.ObjectKey) time.Duration {
	next := r.pollInterval()

	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	for _, wait := range r.waiting[key] {
		if left := earlierMarkGrace - now.Sub(wait.since); !wait.over && left > 0 && left < next {
			next = left
		}
	}

	return next
}

// forgetWaits drops the waits of the Cluster's Machines that are no longer
// as they were when the wait began: marked again, seen unmarked by their
// checks, or gone. Given no Machines it drops them all, which is for a
// Cluster whose checks cannot have been at work.
func (r *ClusterReconciler) forgetWaits(key client.ObjectKey, machines map[string]*clusterv1.Machine) {
	r.mu.Lock()
	defer r.mu.Unlock()

	pending := map[string]bool{}
	for _, m := range machines {
		if capi.EarlierMarkPending(m) {
			pending[m.Name] = true
		}
	}
	for machine := range r.waiting[key] {
		if !pending[machine] {
			delete(r.waiting[key], machine)
		}
	}
	if len(r.waiting[key]) == 0 {
		delete(r.waiting, key)
	}
}

// noteMode remembers the mode the Cluster is read in and, when that is new,
// logs it and records it as an Event on the Cluster. A change of mode also
// drops what was concluded in the old one: the signals now come from
// another source, and those of the old one have not cleared.
func (r *ClusterReconciler) noteMode(log logr.Logger, cluster *clusterv1.Cluster, mode Mode) {
	key := client.ObjectKeyFromObject(cluster)

	r.mu.Lock()
	previous, known := r.modes[key]
	if r.modes == nil {
		r.modes = map[client.ObjectKey]Mode{}
	}
	r.modes[key] = mode
	if known && previous != mode {
		delete(r.seen, key)
		delete(r.wouldRelease, key)
	}
	r.mu.Unlock()

	if known && previous == mode {
		return
	}

	log.Info("signal source selected", "mode", mode, "previousMode", previous)
	if r.DryRun {
		return
	}
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, EventSignalSourceSelected, ActionSelectSource,
		"%s", capi.EventNote(string(mode)+": "+mode.describe()))
}

// releasableBefore reports whether the last poll of the Cluster already
// found the Machine ready to be released in a dry run.
func (r *ClusterReconciler) releasableBefore(key client.ObjectKey, machine string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.wouldRelease[key][machine]
}

// rememberWouldRelease stores the Machines a dry run found ready to be
// released in this poll of the Cluster.
func (r *ClusterReconciler) rememberWouldRelease(key client.ObjectKey, machines map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.wouldRelease == nil {
		r.wouldRelease = map[client.ObjectKey]map[string]bool{}
	}
	if len(machines) == 0 {
		delete(r.wouldRelease, key)
		return
	}
	r.wouldRelease[key] = machines
}

// watchRequests has the workload cluster's requests wake the Cluster's
// reconcile the moment one becomes this operator's to act on, instead of at
// the next poll. The cluster cache adds a watch of one name once per
// connection to a workload cluster and loses it with the connection, so
// this is called on every reconcile. The watch is served by the informer
// that the cached client already lists the requests from, so it opens no
// further connection to the workload cluster.
//
// Only a cluster in ModeExternalRemediationRequest is watched: a watch on a
// resource that the cluster does not serve keeps retrying, and logging an
// error, until the resource is served or the manager stops, whatever
// becomes of the connection it was added on.
func (r *ClusterReconciler) watchRequests(ctx context.Context, cluster client.ObjectKey) error {
	if r.requestWatcher == nil {
		return errors.New("the controller was not set up with a manager, so there is nothing to wake")
	}

	request := &unstructured.Unstructured{}
	request.SetGroupVersionKind(extrr.GroupVersionKind)

	return r.ClusterCache.Watch(ctx, cluster, clustercache.NewWatcher(clustercache.WatcherOptions{
		Name:    requestWatchName,
		Watcher: r.requestWatcher,
		Kind:    request,
		EventHandler: handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []ctrl.Request {
			return []ctrl.Request{{NamespacedName: cluster}}
		}),
		Predicates: []predicate.Predicate{requestBecamePending()},
	}))
}

// requestBecamePending passes the events that make a request this
// operator's to act on: a request that is pending when it is first seen,
// which is every pending one when the watch starts, and one that becomes
// pending, which is NVSentinel releasing its node. Everything else about a
// request is left to the poll: an answer is this operator's own write, and
// a request that is deleted has nothing to be done for it that cannot wait.
func requestBecamePending() predicate.Predicate {
	pending := func(o client.Object) bool {
		request, ok := o.(*unstructured.Unstructured)
		return ok && extrr.Pending(request)
	}

	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return pending(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return !pending(e.ObjectOld) && pending(e.ObjectNew) },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// SetupWithManager registers the controller. It reconciles on Cluster
// creation, spec changes and paused transitions, and whenever the cluster
// cache connects to or loses a workload cluster; otherwise it polls, apart
// from the requests Reconcile has it watch (see watchRequests). The
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

	c, err := b.Build(r)
	if err != nil {
		return err
	}
	r.requestWatcher = c

	return nil
}

// clusterToRequest maps a Cluster event from the cluster cache to its own
// reconcile request.
func clusterToRequest(_ context.Context, o client.Object) []ctrl.Request {
	return []ctrl.Request{{NamespacedName: client.ObjectKeyFromObject(o)}}
}
