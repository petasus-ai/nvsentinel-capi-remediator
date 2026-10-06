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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// newMachine builds a worker Machine of cluster "gpu" and applies mutations.
func newMachine(name string, mutate ...func(*clusterv1.Machine)) *clusterv1.Machine {
	m := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: "gpu"},
		},
		Spec: clusterv1.MachineSpec{ClusterName: "gpu"},
	}
	for _, f := range mutate {
		f(m)
	}

	return m
}

// deleting marks the Machine as being deleted. A finalizer is required
// because the fake client, like the API server, refuses an object that has a
// deletion timestamp but nothing holding it.
func deleting(m *clusterv1.Machine) {
	now := metav1.Now()
	m.DeletionTimestamp = &now
	m.Finalizers = []string{clusterv1.MachineFinalizer}
}

func controlPlane(m *clusterv1.Machine) {
	m.Labels[clusterv1.MachineControlPlaneLabel] = ""
}

func annotate(key string) func(*clusterv1.Machine) {
	return func(m *clusterv1.Machine) {
		if m.Annotations == nil {
			m.Annotations = map[string]string{}
		}
		m.Annotations[key] = ""
	}
}

var (
	marked   = annotate(clusterv1.RemediateMachineAnnotation)
	paused   = annotate(clusterv1.PausedAnnotation)
	optedOut = annotate(clusterv1.MachineSkipRemediationAnnotation)
)

func TestGuard(t *testing.T) {
	tests := []struct {
		name    string
		machine *clusterv1.Machine
		want    SkipReason
	}{
		{"worker", newMachine("w"), ""},
		{"deleting", newMachine("w", deleting), SkipDeleting},
		{"already marked", newMachine("w", marked), SkipAlreadyMarked},
		{"control plane", newMachine("w", controlPlane), SkipControlPlane},
		{"paused", newMachine("w", paused), SkipPaused},
		{"opted out", newMachine("w", optedOut), SkipOptedOut},
		{"deleting outranks marked", newMachine("w", marked, deleting), SkipDeleting},
		{"deleting outranks control plane", newMachine("w", controlPlane, deleting), SkipDeleting},
		{"deleting outranks paused", newMachine("w", paused, deleting), SkipDeleting},
		{"marked outranks control plane", newMachine("w", controlPlane, marked), SkipAlreadyMarked},
		{"marked outranks paused", newMachine("w", paused, marked), SkipAlreadyMarked},
		{"control plane outranks paused", newMachine("w", paused, controlPlane), SkipControlPlane},
		{"paused outranks opted out", newMachine("w", optedOut, paused), SkipPaused},
		{"marked outranks opted out", newMachine("w", optedOut, marked), SkipAlreadyMarked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Guard(tt.machine); got != tt.want {
				t.Fatalf("Guard() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsControlPlaneUsesLabelPresence(t *testing.T) {
	if IsControlPlane(newMachine("w")) {
		t.Fatal("worker reported as control plane")
	}
	// Cluster API sets the label with an empty value; only presence counts.
	if !IsControlPlane(newMachine("cp", controlPlane)) {
		t.Fatal("labelled Machine not reported as control plane")
	}
}

func TestIsMarkedForRemediationUsesAnnotationPresence(t *testing.T) {
	if IsMarkedForRemediation(newMachine("w")) {
		t.Fatal("unmarked Machine reported as marked")
	}
	if !IsMarkedForRemediation(newMachine("w", marked)) {
		t.Fatal("annotated Machine not reported as marked")
	}
}

func TestIsPausedUsesAnnotationPresence(t *testing.T) {
	if IsPaused(newMachine("w")) {
		t.Fatal("running Machine reported as paused")
	}
	if !IsPaused(newMachine("w", paused)) {
		t.Fatal("annotated Machine not reported as paused")
	}
}

func TestIsOptedOutUsesAnnotationPresence(t *testing.T) {
	if IsOptedOut(newMachine("w")) {
		t.Fatal("Machine without the annotation reported as opted out")
	}
	if !IsOptedOut(newMachine("w", optedOut)) {
		t.Fatal("annotated Machine not reported as opted out")
	}
}

func TestEarlierMarkPending(t *testing.T) {
	healthCheck := func(status metav1.ConditionStatus, reason string) func(*clusterv1.Machine) {
		return func(m *clusterv1.Machine) {
			m.Status.Conditions = append(m.Status.Conditions, metav1.Condition{
				Type: clusterv1.MachineHealthCheckSucceededCondition, Status: status, Reason: reason,
			})
		}
	}
	sawTheMark := healthCheck(metav1.ConditionFalse, clusterv1.MachineHealthCheckHasRemediateAnnotationReason)
	marked := func(m *clusterv1.Machine) {
		m.Annotations = map[string]string{clusterv1.RemediateMachineAnnotation: ""}
	}

	tests := []struct {
		name    string
		machine *clusterv1.Machine
		want    bool
	}{
		{"the checks last saw the mark that is gone", newMachine("w", sawTheMark), true},
		// The condition is right while the mark is there.
		{"still marked", newMachine("w", sawTheMark, marked), false},
		{"the checks have seen it unmarked", newMachine("w", healthCheck(metav1.ConditionTrue, "Succeeded")), false},
		{"unhealthy for another reason", newMachine("w", healthCheck(metav1.ConditionFalse, "UnhealthyNode")), false},
		{"never checked", newMachine("w"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EarlierMarkPending(tt.machine); got != tt.want {
				t.Fatalf("EarlierMarkPending() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSkipReasonMessage(t *testing.T) {
	known := []SkipReason{SkipDeleting, SkipAlreadyMarked, SkipControlPlane, SkipPaused, SkipOptedOut, SkipEarlierMark}
	seen := map[string]SkipReason{}
	for _, s := range known {
		msg := s.Message()
		if msg == "" || msg == string(s) {
			t.Errorf("%s has no explanation", s)
		}
		if prev, dup := seen[msg]; dup {
			t.Errorf("%s and %s share the message %q", prev, s, msg)
		}
		seen[msg] = s
	}
	// An unknown reason still produces something readable.
	if got := SkipReason("Other").Message(); got != "Other" {
		t.Fatalf("unknown reason message = %q, want the reason itself", got)
	}
}

func TestSkipReasonInProgress(t *testing.T) {
	inProgress := map[SkipReason]bool{
		SkipDeleting:        true,
		SkipAlreadyMarked:   true,
		SkipControlPlane:    false,
		SkipPaused:          false,
		SkipOptedOut:        false,
		SkipReason("Other"): false,
	}
	for s, want := range inProgress {
		if got := s.InProgress(); got != want {
			t.Errorf("%s.InProgress() = %v, want %v", s, got, want)
		}
	}
}
