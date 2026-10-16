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

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/annotations"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/capi"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/quarantine"
)

// QuarantineControllerName names the controller that replaces Machines on
// request, in logs and metrics.
const QuarantineControllerName = ControllerName + "-quarantine"

// Event reasons and the action recorded by the quarantine controller.
const (
	// ActionQuarantine is the action of Events about a replace request.
	ActionQuarantine = "Quarantine"

	// EventQuarantineRejected is recorded on the Machine when a replace
	// request cannot be carried out as it stands.
	EventQuarantineRejected = "QuarantineRejected"
	// EventDevicesQuarantined is recorded on the Machine once every device
	// of the request is quarantined.
	EventDevicesQuarantined = "DevicesQuarantined"
	// EventMachineReplaced is recorded on the Machine once it is deleted
	// for its MachineSet to replace.
	EventMachineReplaced = "MachineReplaced"
)

// Reasons a replace request is rejected for, next to those of package
// quarantine and the guards of package capi.
const (
	// QuarantineClusterPaused the Machine's Cluster is paused.
	QuarantineClusterPaused quarantine.Reason = "ClusterPaused"
	// QuarantineNotReplaceable no MachineSet owns the Machine, so nothing
	// would replace it once it is deleted.
	QuarantineNotReplaceable quarantine.Reason = "NotReplaceable"
)

// MachineQuarantineReconciler carries out an operator's request to replace a
// Machine whose devices are faulty: it quarantines the DRA devices the
// Machine's workload holds with DeviceTaintRules and, once every rule is in
// place, deletes the Machine for its MachineSet to replace. The replacement
// cannot be given the quarantined devices.
//
// The request is the quarantine.ReplaceAnnotation on the Machine, and how
// far it got is kept in quarantine.ReplaceStatusAnnotation, so that a
// request picked up again after a restart carries on where it stopped. A
// rejected request stays rejected until the status annotation is removed.
type MachineQuarantineReconciler struct {
	// Client reaches the management cluster. Machines and Clusters are read
	// through it.
	Client client.Client
	// Locator finds the devices of a Machine.
	Locator quarantine.Locator
	// Rules creates and checks the rules.
	Rules *quarantine.Rules
	// Drivers are the DRA drivers whose devices a request quarantines.
	Drivers sets.Set[string]
	// Recorder records Events on Machines. Required unless DryRun is set.
	Recorder events.EventRecorder
	// DryRun logs what a request would do and writes nothing: no rule, no
	// annotation, no deletion and no Event.
	DryRun bool
	// ClusterSelector limits the Clusters whose Machines are acted on. Nil
	// selects every Cluster.
	ClusterSelector labels.Selector
}

// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=delete,roleName=device-quarantine-role
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=kubevirtmachines;kubevirtclusters,verbs=get,roleName=device-quarantine-role
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get,roleName=device-quarantine-role
// +kubebuilder:rbac:groups="",resources=pods,verbs=list,roleName=device-quarantine-role
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceclaims,verbs=get,roleName=device-quarantine-role
// +kubebuilder:rbac:groups=resource.k8s.io,resources=devicetaintrules,verbs=get;list;create,roleName=device-quarantine-role

// Reconcile carries out the replace request on one Machine, if it has one.
//
// Every write is conditional on the Machine being the one read here: each
// status patch carries the resourceVersion it was made from, and the
// deletion the resourceVersion of the last patch. A Machine that changed
// meanwhile, with its request edited or withdrawn or the Machine paused,
// fails the write with a conflict, and the request is evaluated again from
// the start. The same keeps a cache that lags behind from acting twice.
func (r *MachineQuarantineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	m := &clusterv1.Machine{}
	if err := r.Client.Get(ctx, req.NamespacedName, m); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	err := r.replace(ctx, m)
	if apierrors.IsConflict(err) {
		// Not every change that makes for a conflict wakes the controller,
		// a newly paused Machine for one, so look again shortly.
		ctrl.LoggerFrom(ctx).V(1).Info("the Machine changed while its replace request was carried out, evaluating it again")
		return ctrl.Result{RequeueAfter: conflictRetry}, nil
	}

	return ctrl.Result{}, err
}

// conflictRetry is how soon a request is evaluated again after the Machine
// changed under it.
const conflictRetry = time.Second

// replace carries out the replace request on the Machine, if it has one.
func (r *MachineQuarantineReconciler) replace(ctx context.Context, m *clusterv1.Machine) error {
	log := ctrl.LoggerFrom(ctx)

	request, requested := m.Annotations[quarantine.ReplaceAnnotation]
	status := m.Annotations[quarantine.ReplaceStatusAnnotation]
	switch {
	case !m.DeletionTimestamp.IsZero():
		// Being replaced already, for this request or otherwise.
		return nil
	case !requested:
		// The request was withdrawn; its status goes with it. Rules
		// already created stay until an operator deletes them.
		if status == "" || r.DryRun {
			return nil
		}

		return r.setStatus(ctx, m, "")
	case quarantine.IsRejected(status):
		log.V(1).Info("replace request rejected before; remove the status annotation to try again", "status", status)
		return nil
	}
	log = log.WithValues("request", request)

	cluster := &clusterv1.Cluster{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: m.Spec.ClusterName}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			// The manager caches only the Clusters of the selector, so this
			// is a Cluster outside it, or one that is gone.
			log.V(1).Info("the Machine's Cluster is not watched, leaving the replace request alone")
			return nil
		}

		return fmt.Errorf("get cluster %s/%s: %w", m.Namespace, m.Spec.ClusterName, err)
	}
	if r.ClusterSelector != nil && !r.ClusterSelector.Matches(labels.Set(cluster.Labels)) {
		return nil
	}

	if reason, message := guardReplace(cluster, m); reason != "" {
		return r.reject(ctx, log, m, request, reason, message)
	}
	names, err := quarantine.ParseRequest(request)
	if err != nil {
		return r.fail(ctx, log, m, request, err)
	}

	// A request picked up again once its rules were made goes straight on
	// to the deletion, without looking the devices up again: the VM may
	// have been given other devices since, and those are not the request's.
	// That holds only while the rules made for the Machine cover the
	// request: one that names a device without such a rule, as after an
	// edit, or a status set by hand, is carried out again.
	var devices []quarantine.Device
	if status == quarantine.StatusReplacing {
		made, err := r.Rules.Of(ctx, quarantine.Owner{Namespace: m.Namespace, Name: m.Name, Cluster: cluster.Name})
		if err != nil {
			return r.fail(ctx, log, m, request, err)
		}
		if quarantine.Covers(made, names) {
			devices = made
		} else {
			log.Info("the rules made for the Machine do not cover its request, carrying it out again", "rules", deviceList(made))
		}
	}
	if devices == nil {
		devices, err = r.quarantine(ctx, log, cluster, m, request, names, status)
		if err != nil || devices == nil {
			return err
		}
	} else if r.DryRun {
		log.Info("dry run: would delete the Machine for its MachineSet to replace", "devices", deviceList(devices))
		return nil
	}

	if err := r.Client.Delete(ctx, m, client.Preconditions{UID: &m.UID, ResourceVersion: &m.ResourceVersion}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return r.fail(ctx, log, m, request, fmt.Errorf("delete machine %s/%s: %w", m.Namespace, m.Name, err))
	}
	log.Info("Machine deleted for its MachineSet to replace")
	r.Recorder.Eventf(m, nil, corev1.EventTypeWarning, EventMachineReplaced, ActionQuarantine, "%s",
		capi.EventNote("Deleted for its MachineSet to replace, with "+deviceList(devices)+" quarantined"))

	return nil
}

// quarantine finds the devices of the request and makes sure each has its
// rule, leaving the Machine's status at StatusReplacing once every rule has
// been read back. It returns the devices, or none when the request was
// rejected or this is a dry run.
//
// A request carried out again from StatusReplacing keeps that status: it is
// already past the point the status tells, and writing it down and up again
// would wake the controller twice, ahead of the backoff of a failing
// deletion.
func (r *MachineQuarantineReconciler) quarantine(
	ctx context.Context, log logr.Logger, cluster *clusterv1.Cluster, m *clusterv1.Machine, request string, names []string,
	status string,
) ([]quarantine.Device, error) {
	held, err := r.Locator.Devices(ctx, cluster, m)
	if err != nil {
		return nil, r.fail(ctx, log, m, request, err)
	}
	devices, err := quarantine.Select(held, r.Drivers, names)
	if err != nil {
		return nil, r.fail(ctx, log, m, request, err)
	}
	log = log.WithValues("devices", deviceList(devices))

	if r.DryRun {
		log.Info("dry run: would quarantine the devices and delete the Machine for its MachineSet to replace")
		return nil, nil
	}

	if status != quarantine.StatusReplacing {
		if err := r.setStatus(ctx, m, quarantine.StatusQuarantining); err != nil {
			return nil, err
		}
	}
	owner := quarantine.Owner{Namespace: m.Namespace, Name: m.Name, Cluster: cluster.Name}
	for _, d := range devices {
		created, err := r.Rules.Ensure(ctx, d, owner)
		if err != nil {
			return nil, r.fail(ctx, log, m, request, err)
		}
		if created {
			log.Info("device quarantined", "device", d.String(), "rule", quarantine.RuleName(d))
		}
	}
	// The scheduler applies a rule as soon as it sees it, so the rules being
	// there is all the Machine waits for.
	for _, d := range devices {
		ok, err := r.Rules.Exists(ctx, d)
		if err != nil {
			return nil, r.fail(ctx, log, m, request, err)
		}
		if !ok {
			return nil, fmt.Errorf("DeviceTaintRule %s for %s is missing right after it was made", quarantine.RuleName(d), d)
		}
	}

	if status != quarantine.StatusReplacing {
		if err := r.setStatus(ctx, m, quarantine.StatusReplacing); err != nil {
			return nil, err
		}
	}
	r.Recorder.Eventf(m, nil, corev1.EventTypeWarning, EventDevicesQuarantined, ActionQuarantine, "%s",
		capi.EventNote("Quarantined with DeviceTaintRules: "+ruleList(devices)))

	return devices, nil
}

// guardReplace returns why the Machine may not be replaced, or "" when it
// may. It applies the guards of package capi, which a Machine marked for
// remediation fails too: its VM may be restarted or replaced meanwhile, and
// a restarted VM may hold other devices than the faulty ones.
func guardReplace(cluster *clusterv1.Cluster, m *clusterv1.Machine) (quarantine.Reason, string) {
	var skip capi.SkipReason
	switch {
	case annotations.IsPaused(cluster, cluster):
		return QuarantineClusterPaused, "the Cluster is paused"
	case capi.IsControlPlane(m):
		skip = capi.SkipControlPlane
	case capi.IsMarkedForRemediation(m):
		return quarantine.Reason(capi.SkipAlreadyMarked),
			"the Machine is marked for remediation, so its VM may be restarted with other devices or replaced meanwhile; " +
				"ask again once the mark is gone"
	case capi.IsPaused(m):
		skip = capi.SkipPaused
	case capi.IsOptedOut(m):
		skip = capi.SkipOptedOut
	case !ownedByMachineSet(m):
		return QuarantineNotReplaceable, "no MachineSet owns the Machine, so nothing would replace it"
	default:
		return "", ""
	}

	return quarantine.Reason(skip), skip.Message()
}

// ownedByMachineSet reports whether a MachineSet controls the Machine.
func ownedByMachineSet(m *clusterv1.Machine) bool {
	ref := metav1.GetControllerOf(m)
	if ref == nil || ref.Kind != "MachineSet" {
		return false
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)

	return err == nil && gv.Group == clusterv1.GroupVersion.Group
}

// fail rejects the request for an error that says it cannot be carried out
// as it stands, and returns any other error for the request to be tried
// again.
func (r *MachineQuarantineReconciler) fail(ctx context.Context, log logr.Logger, m *clusterv1.Machine, request string, err error) error {
	var rejection *quarantine.Rejection
	switch {
	case apierrors.IsConflict(err):
		return err
	case errors.As(err, &rejection):
		return r.reject(ctx, log, m, request, rejection.Reason, rejection.Message)
	case apierrors.IsForbidden(err):
		return r.reject(ctx, log, m, request, quarantine.ReasonForbidden, err.Error())
	default:
		return err
	}
}

// reject records why the request was rejected, in the status annotation
// and a Warning Event. Rules created for it before stay.
func (r *MachineQuarantineReconciler) reject(
	ctx context.Context, log logr.Logger, m *clusterv1.Machine, request string, reason quarantine.Reason, message string,
) error {
	log.Info("replace request rejected", "reason", reason, "message", message)
	if r.DryRun {
		return nil
	}

	if err := r.setStatus(ctx, m, quarantine.RejectedStatus(reason)); err != nil {
		return err
	}
	r.Recorder.Eventf(m, nil, corev1.EventTypeWarning, EventQuarantineRejected, ActionQuarantine, "%s",
		capi.EventNote(fmt.Sprintf("Replace request %q rejected (%s): %s", request, reason, message)))

	return nil
}

// setStatus sets the status annotation, or removes it when status is
// empty, on the Machine as it was read. The Machine is updated in place only
// when the patch succeeded.
func (r *MachineQuarantineReconciler) setStatus(ctx context.Context, m *clusterv1.Machine, status string) error {
	current, set := m.Annotations[quarantine.ReplaceStatusAnnotation]
	if (status == "" && !set) || (set && current == status) {
		return nil
	}

	updated := m.DeepCopy()
	if status == "" {
		delete(updated.Annotations, quarantine.ReplaceStatusAnnotation)
	} else {
		if updated.Annotations == nil {
			updated.Annotations = map[string]string{}
		}
		updated.Annotations[quarantine.ReplaceStatusAnnotation] = status
	}
	// The resourceVersion of m makes the patch fail with a conflict once
	// the Machine has changed since it was read.
	if err := r.Client.Patch(ctx, updated, client.MergeFromWithOptions(m, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("patch machine %s/%s: %w", m.Namespace, m.Name, err)
	}
	*m = *updated

	return nil
}

// deviceList names the devices as driver/pool/device.
func deviceList(ds []quarantine.Device) string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.String()
	}

	return strings.Join(out, ", ")
}

// ruleList names each device with its rule.
func ruleList(ds []quarantine.Device) string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.String() + " (" + quarantine.RuleName(d) + ")"
	}

	return strings.Join(out, ", ")
}

// replaceRequestChanged admits a Machine that carries a replace request or
// its status when it is first seen, and one whose request or status changed
// afterwards. Nothing else about a Machine bears on its request.
func replaceRequestChanged() predicate.Predicate {
	keys := []string{quarantine.ReplaceAnnotation, quarantine.ReplaceStatusAnnotation}

	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			for _, k := range keys {
				if _, ok := e.Object.GetAnnotations()[k]; ok {
					return true
				}
			}

			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			for _, k := range keys {
				was, wasSet := e.ObjectOld.GetAnnotations()[k]
				is, isSet := e.ObjectNew.GetAnnotations()[k]
				if wasSet != isSet || was != is {
					return true
				}
			}

			return false
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// SetupWithManager registers the controller. It reconciles a Machine when
// its replace request or that request's status changes.
func (r *MachineQuarantineReconciler) SetupWithManager(mgr ctrl.Manager, options controller.Options) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(QuarantineControllerName).
		For(&clusterv1.Machine{}, builder.WithPredicates(replaceRequestChanged())).
		WithOptions(options).
		Complete(r)
}
