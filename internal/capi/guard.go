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

import clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

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
	default:
		return ""
	}
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
