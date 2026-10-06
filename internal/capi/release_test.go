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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

const releaseReason = "the signal cleared"

// markedBy marks the Machine the way MarkForRemediation does.
func markedBy(action string) func(*clusterv1.Machine) {
	return func(m *clusterv1.Machine) {
		if m.Annotations == nil {
			m.Annotations = map[string]string{}
		}
		m.Annotations[clusterv1.RemediateMachineAnnotation] = ""
		m.Annotations[RemediationReasonAnnotation] = testReason
		m.Annotations[RemediationActionAnnotation] = action
	}
}

func TestMarkedAction(t *testing.T) {
	tests := []struct {
		name    string
		machine *clusterv1.Machine
		action  string
		ok      bool
	}{
		{"unmarked", newMachine("w"), "", false},
		{"marked by someone else", newMachine("w", marked), "", false},
		{"marked for a restart", newMachine("w", markedBy(ActionRestart)), ActionRestart, true},
		{"marked for a replacement", newMachine("w", markedBy(ActionRemediate)), ActionRemediate, true},
		// Someone removed the remediate-machine annotation: nothing is
		// pending any more, whatever our leftovers say.
		{"leftover action", newMachine("w", annotate(RemediationActionAnnotation)), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action, ok := MarkedAction(tt.machine)
			if action != tt.action || ok != tt.ok {
				t.Fatalf("MarkedAction() = %q, %v; want %q, %v", action, ok, tt.action, tt.ok)
			}
		})
	}
}

func TestMarkThenReleaseRoundTrips(t *testing.T) {
	m := newMachine("w", func(m *clusterv1.Machine) {
		m.Annotations = map[string]string{"keep": "me"}
	})
	a, rec := newActuator(t, m)

	if _, err := a.MarkForRemediation(context.Background(), m, ActionRestart, testReason, RestartRecord{}); err != nil {
		t.Fatalf("MarkForRemediation: %v", err)
	}
	<-rec.Events

	released, err := a.Release(context.Background(), m, releaseReason)
	if err != nil || !released {
		t.Fatalf("Release = %v, %v; want released", released, err)
	}

	for name, got := range map[string]*clusterv1.Machine{"stored": stored(t, a.Client, m), "in memory": m} {
		for _, key := range []string{clusterv1.RemediateMachineAnnotation, RemediationReasonAnnotation, RemediationActionAnnotation} {
			if _, ok := got.Annotations[key]; ok {
				t.Errorf("%s: %s still set", name, key)
			}
		}
		if got.Annotations["keep"] != "me" {
			t.Errorf("%s: unrelated annotation lost", name)
		}
	}
	assertEvent(t, rec, "Normal RemediationReleased Released from remediation: "+releaseReason)
	assertNoEvent(t, rec)
}

func TestReleaseLeavesOtherMachinesAlone(t *testing.T) {
	tests := []struct {
		name    string
		machine *clusterv1.Machine
	}{
		{"unmarked", newMachine("w")},
		{"marked by someone else", newMachine("w", marked)},
		{"deleting", newMachine("w", markedBy(ActionRestart), deleting)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, rec := newActuator(t, tt.machine)
			before := stored(t, a.Client, tt.machine)

			released, err := a.Release(context.Background(), tt.machine, releaseReason)
			if err != nil || released {
				t.Fatalf("Release = %v, %v; want nothing released", released, err)
			}
			if stored(t, a.Client, tt.machine).ResourceVersion != before.ResourceVersion {
				t.Fatal("Machine was written to")
			}
			assertNoEvent(t, rec)
		})
	}
}

func TestReleaseDryRunWritesNothing(t *testing.T) {
	m := newMachine("w", markedBy(ActionRestart))
	a, rec := newActuator(t, m)
	a.DryRun = true
	before := stored(t, a.Client, m)

	released, err := a.Release(context.Background(), m, releaseReason)
	if err != nil || !released {
		t.Fatalf("Release = %v, %v; want the would-release result", released, err)
	}
	after := stored(t, a.Client, m)
	if after.ResourceVersion != before.ResourceVersion || !IsMarkedForRemediation(after) || !IsMarkedForRemediation(m) {
		t.Fatal("dry run wrote to the Machine")
	}
	assertNoEvent(t, rec)
}

func TestReleaseRefusesAStaleMachine(t *testing.T) {
	m := newMachine("w", markedBy(ActionRestart))
	a, rec := newActuator(t, m)

	// Someone changes the Machine after this copy was read: the release
	// must not overwrite what it has not seen.
	fresh := stored(t, a.Client, m)
	fresh.Annotations["changed"] = "later"
	if err := a.Client.Update(context.Background(), fresh); err != nil {
		t.Fatalf("update: %v", err)
	}

	released, err := a.Release(context.Background(), m, releaseReason)
	if !apierrors.IsConflict(err) || released {
		t.Fatalf("Release = %v, %v; want a conflict", released, err)
	}
	if !IsMarkedForRemediation(m) || !IsMarkedForRemediation(stored(t, a.Client, m)) {
		t.Fatal("a failed release changed the Machine")
	}
	assertNoEvent(t, rec)
}

func TestEscalateTurnsARestartMarkIntoAReplacement(t *testing.T) {
	m := newMachine("w", markedBy(ActionRestart))
	a, rec := newActuator(t, m)

	escalated, err := a.Escalate(context.Background(), m, "a replacement is called for")
	if err != nil || !escalated {
		t.Fatalf("Escalate = %v, %v; want escalated", escalated, err)
	}
	for name, got := range map[string]*clusterv1.Machine{"stored": stored(t, a.Client, m), "in memory": m} {
		if action, ok := MarkedAction(got); !ok || action != ActionRemediate {
			t.Errorf("%s: MarkedAction = %q, %v; want %q", name, action, ok, ActionRemediate)
		}
		if got.Annotations[RemediationReasonAnnotation] != "a replacement is called for" {
			t.Errorf("%s: reason not updated", name)
		}
	}
	assertEvent(t, rec, "Warning MarkedForRemediation Restart escalated to a replacement: a replacement is called for")

	// A replacement mark is final: nothing is escalated again.
	if escalated, err := a.Escalate(context.Background(), m, "again"); err != nil || escalated {
		t.Fatalf("second Escalate = %v, %v; want nothing", escalated, err)
	}
	assertNoEvent(t, rec)
}

func TestEscalateLeavesOtherMachinesAlone(t *testing.T) {
	tests := []struct {
		name    string
		machine *clusterv1.Machine
	}{
		{"unmarked", newMachine("w")},
		{"marked by someone else", newMachine("w", marked)},
		{"marked for a replacement", newMachine("w", markedBy(ActionRemediate))},
		{"deleting", newMachine("w", markedBy(ActionRestart), deleting)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, rec := newActuator(t, tt.machine)
			before := stored(t, a.Client, tt.machine)

			if escalated, err := a.Escalate(context.Background(), tt.machine, "x"); err != nil || escalated {
				t.Fatalf("Escalate = %v, %v; want nothing", escalated, err)
			}
			if stored(t, a.Client, tt.machine).ResourceVersion != before.ResourceVersion {
				t.Fatal("Machine was written to")
			}
			assertNoEvent(t, rec)
		})
	}
}

func TestEscalateDryRunWritesNothing(t *testing.T) {
	m := newMachine("w", markedBy(ActionRestart))
	a, rec := newActuator(t, m)
	a.DryRun = true

	if escalated, err := a.Escalate(context.Background(), m, "x"); err != nil || !escalated {
		t.Fatalf("Escalate = %v, %v; want the would-escalate result", escalated, err)
	}
	if action, _ := MarkedAction(stored(t, a.Client, m)); action != ActionRestart {
		t.Fatal("dry run wrote to the Machine")
	}
	assertNoEvent(t, rec)
}

func TestEscalateReportsPatchFailure(t *testing.T) {
	m := newMachine("gone", markedBy(ActionRestart))
	a, rec := newActuator(t)

	if escalated, err := a.Escalate(context.Background(), m, "x"); !apierrors.IsNotFound(err) || escalated {
		t.Fatalf("Escalate = %v, %v; want NotFound", escalated, err)
	}
	if action, _ := MarkedAction(m); action != ActionRestart {
		t.Fatal("in-memory Machine changed although the patch failed")
	}
	assertNoEvent(t, rec)
}

func TestMarkRecordsTheBootIDAndReleaseRemovesIt(t *testing.T) {
	m := newMachine("w")
	a, rec := newActuator(t, m)

	if _, err := a.MarkForRemediation(context.Background(), m, ActionRestart, testReason, RestartRecord{BootID: "boot-1"}); err != nil {
		t.Fatalf("MarkForRemediation: %v", err)
	}
	if got := stored(t, a.Client, m).Annotations[RemediationBootIDAnnotation]; got != "boot-1" {
		t.Fatalf("recorded boot ID = %q, want boot-1", got)
	}
	<-rec.Events

	if _, err := a.Release(context.Background(), m, releaseReason); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, ok := stored(t, a.Client, m).Annotations[RemediationBootIDAnnotation]; ok {
		t.Fatal("boot ID left behind")
	}
}

func TestMarkRecordsTheRequestAndReleaseRemovesIt(t *testing.T) {
	m := newMachine("w")
	a, rec := newActuator(t, m)

	if _, err := a.MarkForRemediation(context.Background(), m, ActionRestart, testReason, RestartRecord{BootID: "boot-1", Request: "extrr-1"}); err != nil {
		t.Fatalf("MarkForRemediation: %v", err)
	}
	if got := stored(t, a.Client, m).Annotations[RemediationRequestAnnotation]; got != "extrr-1" {
		t.Fatalf("recorded request = %q, want extrr-1", got)
	}
	<-rec.Events

	if _, err := a.Release(context.Background(), m, releaseReason); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, ok := stored(t, a.Client, m).Annotations[RemediationRequestAnnotation]; ok {
		t.Fatal("request left behind")
	}
}

func TestMarkWithoutRequestRecordsNone(t *testing.T) {
	m := newMachine("w")
	a, _ := newActuator(t, m)

	if _, err := a.MarkForRemediation(context.Background(), m, ActionRestart, testReason, RestartRecord{BootID: "boot-1"}); err != nil {
		t.Fatalf("MarkForRemediation: %v", err)
	}
	if _, ok := stored(t, a.Client, m).Annotations[RemediationRequestAnnotation]; ok {
		t.Fatal("a request recorded for a mark made for none")
	}
}

func TestRestartAnswers(t *testing.T) {
	askedFor := func(request string) *clusterv1.Machine {
		return newMachine("w", markedBy(ActionRestart), func(m *clusterv1.Machine) {
			m.Annotations[RemediationRequestAnnotation] = request
		})
	}
	tests := []struct {
		name    string
		machine *clusterv1.Machine
		want    bool
	}{
		{"the request the restart was asked for", askedFor("extrr-1"), true},
		{"another request", askedFor("extrr-0"), false},
		// Made by an earlier version, which did not record the request, or for a node
		// condition.
		{"a mark that names no request", newMachine("w", markedBy(ActionRestart)), true},
		{"a Machine without annotations", newMachine("w"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RestartAnswers(tt.machine, "extrr-1"); got != tt.want {
				t.Fatalf("RestartAnswers() = %v, want %v", got, tt.want)
			}
		})
	}
}

// What was recorded for a restart says nothing about a replacement.
func TestEscalateDropsWhatWasRecordedForTheRestart(t *testing.T) {
	m := newMachine("w")
	a, rec := newActuator(t, m)
	if _, err := a.MarkForRemediation(context.Background(), m, ActionRestart, testReason, RestartRecord{BootID: "boot-1", Request: "extrr-1"}); err != nil {
		t.Fatalf("MarkForRemediation: %v", err)
	}
	<-rec.Events

	if escalated, err := a.Escalate(context.Background(), m, "a replacement is called for"); err != nil || !escalated {
		t.Fatalf("Escalate = %v, %v; want escalated", escalated, err)
	}
	got := stored(t, a.Client, m)
	for _, key := range []string{RemediationBootIDAnnotation, RemediationRequestAnnotation} {
		if _, ok := got.Annotations[key]; ok {
			t.Errorf("%s kept on a replacement mark", key)
		}
	}
}

func TestMarkWithoutBootIDRecordsNone(t *testing.T) {
	m := newMachine("w")
	a, _ := newActuator(t, m)

	if _, err := a.MarkForRemediation(context.Background(), m, ActionRemediate, testReason, RestartRecord{}); err != nil {
		t.Fatalf("MarkForRemediation: %v", err)
	}
	if _, ok := stored(t, a.Client, m).Annotations[RemediationBootIDAnnotation]; ok {
		t.Fatal("empty boot ID recorded")
	}
}

func TestRestartCompleted(t *testing.T) {
	restarting := func(bootID string) func(*clusterv1.Machine) {
		return func(m *clusterv1.Machine) {
			markedBy(ActionRestart)(m)
			if bootID != "" {
				m.Annotations[RemediationBootIDAnnotation] = bootID
			}
		}
	}
	node := func(bootID string, ready corev1.ConditionStatus) *corev1.Node {
		n := &corev1.Node{}
		n.Status.NodeInfo.BootID = bootID
		if ready != "" {
			n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}}
		}
		return n
	}

	tests := []struct {
		name    string
		machine *clusterv1.Machine
		node    *corev1.Node
		want    bool
	}{
		{"restarted and Ready", newMachine("w", restarting("boot-1")), node("boot-2", corev1.ConditionTrue), true},
		{"not restarted yet", newMachine("w", restarting("boot-1")), node("boot-1", corev1.ConditionTrue), false},
		{"restarted, not Ready", newMachine("w", restarting("boot-1")), node("boot-2", corev1.ConditionFalse), false},
		{"restarted, no Ready condition", newMachine("w", restarting("boot-1")), node("boot-2", ""), false},
		{"no recorded boot ID", newMachine("w", restarting("")), node("boot-2", corev1.ConditionTrue), false},
		{"node reports no boot ID", newMachine("w", restarting("boot-1")), node("", corev1.ConditionTrue), false},
		{"no node", newMachine("w", restarting("boot-1")), nil, false},
		// Only a restart this operator asked for completes this way.
		{"marked for a replacement", newMachine("w", markedBy(ActionRemediate), func(m *clusterv1.Machine) {
			m.Annotations[RemediationBootIDAnnotation] = "boot-1"
		}), node("boot-2", corev1.ConditionTrue), false},
		{"not marked", newMachine("w"), node("boot-2", corev1.ConditionTrue), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RestartCompleted(tt.machine, tt.node); got != tt.want {
				t.Fatalf("RestartCompleted() = %v, want %v", got, tt.want)
			}
		})
	}
}
