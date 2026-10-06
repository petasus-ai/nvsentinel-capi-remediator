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

package capi

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// SkipReason names why a Machine was left untouched. The zero value means
// the Machine may be remediated.
type SkipReason string

const (
	// SkipDeleting the Machine is already being deleted, so remediating it
	// would only race the deletion.
	SkipDeleting SkipReason = "MachineDeleting"
	// SkipAlreadyMarked the Machine already carries the remediate-machine
	// annotation and Cluster API is working through it. Re-stamping it would
	// only churn the object.
	SkipAlreadyMarked SkipReason = "AlreadyMarkedForRemediation"
	// SkipControlPlane control plane Machines are never remediated by this
	// operator. Their remediation has a far larger blast radius and its own
	// semantics in the control plane provider, so that decision is left to
	// an operator.
	SkipControlPlane SkipReason = "ControlPlaneMachine"
	// SkipPaused the Machine carries the paused annotation. Cluster API
	// would hold on to the remediate-machine annotation and act at some
	// arbitrary time after the Machine is unpaused, so the signal is left to
	// be evaluated again then instead.
	SkipPaused SkipReason = "MachinePaused"
	// SkipOptedOut the Machine carries Cluster API's skip-remediation
	// annotation. MachineHealthChecks leave such a Machine alone, so the
	// remediate-machine annotation would do nothing until someone removes
	// the opt-out and then act at an arbitrary time.
	SkipOptedOut SkipReason = "MachineOptedOutOfRemediation"
	// SkipEarlierMark the Machine is no longer marked, but its
	// MachineHealthChecks may still hold what they kept for the mark it
	// carried (see EarlierMarkPending). A restart is held back a while for
	// them to drop it.
	SkipEarlierMark SkipReason = "EarlierRemediationNotCleared"
)

// Message explains the reason in a clause, for logs and Events.
func (s SkipReason) Message() string {
	switch s {
	case SkipDeleting:
		return "the Machine is already being deleted"
	case SkipAlreadyMarked:
		return "the Machine is already marked for remediation"
	case SkipControlPlane:
		return "the Machine is a control plane member"
	case SkipPaused:
		return "the Machine is paused"
	case SkipOptedOut:
		return "the Machine has opted out of remediation"
	case SkipEarlierMark:
		return "the MachineHealthChecks still show the mark the Machine carried before"
	default:
		return string(s)
	}
}

// InProgress reports whether the reason means remediation is already under
// way, so that skipping loses nothing: the Machine is being deleted, or
// Cluster API is already working through the annotation.
func (s SkipReason) InProgress() bool {
	return s == SkipDeleting || s == SkipAlreadyMarked
}

// Guard reports whether a Machine may be remediated. It returns the reason
// to leave the Machine alone, or the zero value when remediation may
// proceed. Every action runs it first; callers may run it themselves to
// explain what a dry run would have done. Pausing at the Cluster level is
// not visible on the Machine and is the caller's job.
func Guard(m *clusterv1.Machine) SkipReason {
	switch {
	case !m.DeletionTimestamp.IsZero():
		return SkipDeleting
	case IsMarkedForRemediation(m):
		return SkipAlreadyMarked
	case IsControlPlane(m):
		return SkipControlPlane
	case IsPaused(m):
		return SkipPaused
	case IsOptedOut(m):
		return SkipOptedOut
	default:
		return ""
	}
}

// EarlierMarkPending reports whether the Machine's HealthCheckSucceeded
// condition still names a remediate-machine annotation the Machine no
// longer carries: no check has rewritten the condition since the mark was
// removed.
//
// That is the usual sign that a check still holds the provider's
// remediation request of the earlier mark. A check that remediates through
// a template drops that request, and rewrites the condition with it, when
// it next finds the Machine healthy, and it looks at intervals, not at
// every change. A mark set again before that is taken for the old one: the
// earlier request stays, and a provider that has already restarted the
// Machine for it gives the Machine up instead of restarting it again.
//
// It is a sign and not proof, in both directions. Such a check leaves the
// condition of a healthy Machine alone when it has no request to delete
// for it, so a Machine whose earlier mark never led to one, or lost it
// some other way, can keep the condition for good; whoever waits on this
// must not wait without end. And the condition may read cleared while a
// request is still there, as when several checks cover the Machine, the
// request has a finalizer, or a check is over its limit of unhealthy
// Machines, which has it rewrite the condition and delete nothing.
func EarlierMarkPending(m *clusterv1.Machine) bool {
	if IsMarkedForRemediation(m) {
		return false
	}
	c := meta.FindStatusCondition(m.Status.Conditions, clusterv1.MachineHealthCheckSucceededCondition)

	return c != nil && c.Status == metav1.ConditionFalse && c.Reason == clusterv1.MachineHealthCheckHasRemediateAnnotationReason
}

// IsControlPlane reports whether the Machine belongs to the control plane.
// Cluster API labels those Machines; the check mirrors its
// util.IsControlPlaneMachine without depending on the whole module.
func IsControlPlane(m *clusterv1.Machine) bool {
	_, ok := m.Labels[clusterv1.MachineControlPlaneLabel]
	return ok
}

// IsMarkedForRemediation reports whether the remediate-machine annotation is
// present. Cluster API reads only its presence, never its value.
func IsMarkedForRemediation(m *clusterv1.Machine) bool {
	_, ok := m.Annotations[clusterv1.RemediateMachineAnnotation]
	return ok
}

// IsPaused reports whether the Machine carries the paused annotation, which
// makes Cluster API defer any remediation of it.
func IsPaused(m *clusterv1.Machine) bool {
	_, ok := m.Annotations[clusterv1.PausedAnnotation]
	return ok
}

// IsOptedOut reports whether the Machine carries the skip-remediation
// annotation, which makes MachineHealthChecks never remediate it.
func IsOptedOut(m *clusterv1.Machine) bool {
	_, ok := m.Annotations[clusterv1.MachineSkipRemediationAnnotation]
	return ok
}
