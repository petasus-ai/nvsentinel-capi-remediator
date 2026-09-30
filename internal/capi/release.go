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

// MarkedAction returns the action this operator asked for when it marked
// the Machine, and false when the Machine is not marked or was marked by
// someone else. The annotations live on the Machine, so the answer
// survives a restart of the operator.
func MarkedAction(m *clusterv1.Machine) (string, bool) {
	if !IsMarkedForRemediation(m) {
		return "", false
	}
	action, ok := m.Annotations[RemediationActionAnnotation]

	return action, ok
}

// Release removes the remediate-machine annotation this operator set,
// together with its own annotations, and records why in an Event. Cluster
// API never removes the annotation itself: while it stays, every
// MachineHealthCheck selecting the Machine keeps it unhealthy, so a
// provider that has restarted the Machine retries until it gives up and
// the Machine is replaced after all. Once the annotation is gone the
// checks judge the Machine by their own checks again and delete the
// remediation requests of a healthy Machine.
//
// It reports whether the Machine was released. A Machine this operator did
// not mark, or one being deleted, is left alone; in dry run nothing is
// written. A paused Machine is released too: the release only withdraws a
// request, and keeping it would have the MachineHealthChecks act on it the
// moment the Machine is unpaused. The Machine is updated in place only when
// the patch succeeded.
func (a *Actuator) Release(ctx context.Context, m *clusterv1.Machine, reason string) (bool, error) {
	action, ok := MarkedAction(m)
	if !ok || !m.DeletionTimestamp.IsZero() {
		return false, nil
	}

	if a.DryRun {
		return true, nil
	}

	updated := m.DeepCopy()
	delete(updated.Annotations, clusterv1.RemediateMachineAnnotation)
	delete(updated.Annotations, RemediationReasonAnnotation)
	delete(updated.Annotations, RemediationActionAnnotation)
	delete(updated.Annotations, RemediationBootIDAnnotation)

	// The resource version makes the patch fail rather than remove an
	// annotation someone else set again after the Machine was read.
	if err := a.Client.Patch(ctx, updated, client.MergeFromWithOptions(m, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, fmt.Errorf("patch machine %s/%s: %w", m.Namespace, m.Name, err)
	}
	*m = *updated

	a.Recorder.Eventf(m, nil, corev1.EventTypeNormal, EventRemediationReleased, action,
		"%s", EventNote("Released from remediation: "+reason))

	return true, nil
}

// RestartCompleted reports whether the node behind a Machine this operator
// marked for a restart has restarted since and is back: it reports another
// boot ID than the one recorded with the mark, and it is Ready. A mark
// without a recorded boot ID, or a node that reports none, never counts as
// restarted, since a restart could not be told apart.
func RestartCompleted(m *clusterv1.Machine, node *corev1.Node) bool {
	if action, ok := MarkedAction(m); !ok || action != ActionRestart || node == nil {
		return false
	}

	recorded := m.Annotations[RemediationBootIDAnnotation]
	current := node.Status.NodeInfo.BootID
	if recorded == "" || current == "" || current == recorded {
		return false
	}

	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}

	return false
}

// Escalate turns this operator's restart mark into a replacement mark, so
// that the Machine is never released: a replacement stays requested until
// Cluster API replaces the Machine, even once a restart has lowered the
// signal. It reports whether the mark changed, and changes nothing in dry
// run or on a Machine not marked by this operator for a restart. The
// Machine is updated in place only when the patch succeeded.
func (a *Actuator) Escalate(ctx context.Context, m *clusterv1.Machine, reason string) (bool, error) {
	if action, ok := MarkedAction(m); !ok || action != ActionRestart || !m.DeletionTimestamp.IsZero() {
		return false, nil
	}

	if a.DryRun {
		return true, nil
	}

	updated := m.DeepCopy()
	updated.Annotations[RemediationActionAnnotation] = ActionRemediate
	updated.Annotations[RemediationReasonAnnotation] = reason

	if err := a.Client.Patch(ctx, updated, client.MergeFromWithOptions(m, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, fmt.Errorf("patch machine %s/%s: %w", m.Namespace, m.Name, err)
	}
	*m = *updated

	a.Recorder.Eventf(m, nil, corev1.EventTypeWarning, EventMarkedForRemediation, ActionRemediate,
		"%s", EventNote("Restart escalated to a replacement: "+reason))

	return true, nil
}
