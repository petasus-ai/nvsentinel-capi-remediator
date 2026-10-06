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
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RestartRecord is what is recorded with a mark that asks for a restart.
// The zero value records nothing, which is what a replacement takes.
type RestartRecord struct {
	// BootID is the boot ID the Machine's node reports now. The restart has
	// happened once the node reports another one.
	BootID string
	// Request names the ExternalRemediationRequest the restart is asked
	// for, which is the one it answers. Empty when it is asked for a node
	// condition.
	Request string
}

// MarkForRemediation hands the Machine to Cluster API by stamping the
// remediate-machine annotation, with the reason recorded next to it and in
// an Event. What follows is decided by the MachineHealthChecks covering the
// Machine (see Coverage), subject to their own gates such as maxUnhealthy.
// The action names what the caller expects of it, Remediate or Restart, and
// is recorded with the mark, and so is what a RestartRecord holds, so that
// a restart can later be told apart and credited (see RestartCompleted and
// RestartAnswers). It returns the SkipReason when a guard left the Machine
// untouched, and the zero value once the annotation is in place or, in dry
// run, would have been. The Machine is updated in place only when the patch
// succeeded.
//
// Cluster API's own event for this names the MachineHealthCheck that acted,
// not the fault, and the annotations disappear with the Machine moments
// later. The Event recorded here is what an operator still has afterwards.
func (a *Actuator) MarkForRemediation(
	ctx context.Context, m *clusterv1.Machine, action, reason string, restart RestartRecord,
) (SkipReason, error) {
	if skip := Guard(m); skip != "" {
		return skip, nil
	}

	if a.DryRun {
		return "", nil
	}

	updated := m.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[clusterv1.RemediateMachineAnnotation] = ""
	updated.Annotations[RemediationReasonAnnotation] = reason
	updated.Annotations[RemediationActionAnnotation] = action
	if restart.BootID != "" {
		updated.Annotations[RemediationBootIDAnnotation] = restart.BootID
	}
	if restart.Request != "" {
		updated.Annotations[RemediationRequestAnnotation] = restart.Request
	}

	if err := a.Client.Patch(ctx, updated, client.MergeFrom(m)); err != nil {
		return "", fmt.Errorf("patch machine %s/%s: %w", m.Namespace, m.Name, err)
	}
	*m = *updated

	a.Recorder.Eventf(m, nil, corev1.EventTypeWarning, EventMarkedForRemediation, action,
		"%s", EventNote("Marked for remediation: "+reason))

	return "", nil
}

// RecordSkipped records on the Machine that a signal called for remediation
// but a guard stopped it, so that a fault nobody will act on is not dropped
// silently. Reasons that mean remediation is already under way are not
// recorded: they are the expected steady state while Cluster API works, and
// a Warning every poll would only drown the Machine's real Events. Nothing
// is recorded in dry run either. Call it once per Machine and signal:
// repeating it on every poll while the signal persists is the caller's to
// avoid.
func (a *Actuator) RecordSkipped(m *clusterv1.Machine, skip SkipReason, action, reason string) {
	if a.DryRun || skip == "" || skip.InProgress() {
		return
	}

	a.Recorder.Eventf(m, nil, corev1.EventTypeWarning, EventRemediationSkipped, action,
		"%s", EventNote("Remediation skipped because "+skip.Message()+": "+reason))
}
