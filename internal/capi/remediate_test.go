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
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const testReason = "GpuXidError DCGM_FR_XID_ERROR (REPLACE_VM)"

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	return scheme
}

// newActuator returns an Actuator over a fake management cluster holding the
// given Machines, plus the recorder to inspect the Events it emits.
func newActuator(t *testing.T, machines ...*clusterv1.Machine) (*Actuator, *events.FakeRecorder) {
	t.Helper()

	objs := make([]client.Object, 0, len(machines))
	for _, m := range machines {
		objs = append(objs, m)
	}

	rec := events.NewFakeRecorder(8)
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()

	return &Actuator{Client: c, Recorder: rec}, rec
}

// stored fetches the server's copy of the Machine.
func stored(t *testing.T, c client.Client, m *clusterv1.Machine) *clusterv1.Machine {
	t.Helper()

	got := &clusterv1.Machine{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(m), got); err != nil {
		t.Fatalf("get machine: %v", err)
	}

	return got
}

func assertNoEvent(t *testing.T, rec *events.FakeRecorder) {
	t.Helper()

	select {
	case e := <-rec.Events:
		t.Fatalf("unexpected event %q", e)
	default:
	}
}

func assertEvent(t *testing.T, rec *events.FakeRecorder, want string) {
	t.Helper()

	select {
	case e := <-rec.Events:
		if e != want {
			t.Fatalf("event = %q, want %q", e, want)
		}
	default:
		t.Fatalf("no event recorded, want %q", want)
	}
}

func TestMarkForRemediationStampsAnnotationsAndRecordsEvent(t *testing.T) {
	m := newMachine("w", func(m *clusterv1.Machine) {
		m.Annotations = map[string]string{"keep": "me"}
	})
	a, rec := newActuator(t, m)

	skip, err := a.MarkForRemediation(context.Background(), m, testReason)
	if err != nil {
		t.Fatalf("MarkForRemediation: %v", err)
	}
	if skip != "" {
		t.Fatalf("skipped with %q, want the Machine marked", skip)
	}

	for name, got := range map[string]*clusterv1.Machine{"stored": stored(t, a.Client, m), "in memory": m} {
		if v, ok := got.Annotations[clusterv1.RemediateMachineAnnotation]; !ok || v != "" {
			t.Errorf("%s: remediate-machine annotation = %q, %v; want present and empty", name, v, ok)
		}
		if v := got.Annotations[RemediationReasonAnnotation]; v != testReason {
			t.Errorf("%s: reason annotation = %q, want %q", name, v, testReason)
		}
		// A merge patch must leave unrelated annotations in place.
		if v := got.Annotations["keep"]; v != "me" {
			t.Errorf("%s: existing annotation lost: %q", name, v)
		}
	}

	assertEvent(t, rec, "Warning MarkedForRemediation Marked for remediation: "+testReason)
	assertNoEvent(t, rec)
}

func TestMarkForRemediationWithoutExistingAnnotations(t *testing.T) {
	m := newMachine("w")
	a, _ := newActuator(t, m)

	if skip, err := a.MarkForRemediation(context.Background(), m, testReason); err != nil || skip != "" {
		t.Fatalf("MarkForRemediation = %q, %v", skip, err)
	}
	if !IsMarkedForRemediation(stored(t, a.Client, m)) {
		t.Fatal("annotation not stored")
	}
}

func TestMarkForRemediationSendsOnlyTheAnnotations(t *testing.T) {
	m := newMachine("w")
	var (
		gotType types.PatchType
		gotBody []byte
	)
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(m).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				body, err := patch.Data(obj)
				if err != nil {
					return err
				}
				gotType, gotBody = patch.Type(), body

				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	a := &Actuator{Client: c, Recorder: events.NewFakeRecorder(1)}

	if skip, err := a.MarkForRemediation(context.Background(), m, testReason); err != nil || skip != "" {
		t.Fatalf("MarkForRemediation = %q, %v", skip, err)
	}

	if gotType != types.MergePatchType {
		t.Fatalf("patch type = %q, want %q", gotType, types.MergePatchType)
	}
	// Labels and spec are set on the Machine but must never travel in the
	// patch; only the two annotations do.
	want := map[string]any{"metadata": map[string]any{"annotations": map[string]any{
		clusterv1.RemediateMachineAnnotation: "",
		RemediationReasonAnnotation:          testReason,
	}}}
	var got map[string]any
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("patch body %s: %v", gotBody, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("patch body = %s, want %v", gotBody, want)
	}
}

func TestMarkForRemediationDoesNotRestampAMarkedMachine(t *testing.T) {
	m := newMachine("w")
	a, rec := newActuator(t, m)

	if _, err := a.MarkForRemediation(context.Background(), m, testReason); err != nil {
		t.Fatalf("first MarkForRemediation: %v", err)
	}
	<-rec.Events
	before := stored(t, a.Client, m)

	skip, err := a.MarkForRemediation(context.Background(), m, "a later, different reason")
	if err != nil {
		t.Fatalf("second MarkForRemediation: %v", err)
	}
	if skip != SkipAlreadyMarked {
		t.Fatalf("second call skipped with %q, want %q", skip, SkipAlreadyMarked)
	}

	after := stored(t, a.Client, m)
	if after.ResourceVersion != before.ResourceVersion {
		t.Fatal("second call wrote to the Machine")
	}
	if after.Annotations[RemediationReasonAnnotation] != testReason {
		t.Fatal("second call replaced the original reason")
	}
	assertNoEvent(t, rec)
}

func TestMarkForRemediationLeavesGuardedMachinesAlone(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*clusterv1.Machine)
		want   SkipReason
	}{
		{"deleting", deleting, SkipDeleting},
		{"control plane", controlPlane, SkipControlPlane},
		{"paused", paused, SkipPaused},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMachine("m", tt.mutate)
			a, rec := newActuator(t, m)
			before := stored(t, a.Client, m)

			skip, err := a.MarkForRemediation(context.Background(), m, testReason)
			if err != nil {
				t.Fatalf("MarkForRemediation: %v", err)
			}
			if skip != tt.want {
				t.Fatalf("skip = %q, want %q", skip, tt.want)
			}

			after := stored(t, a.Client, m)
			if after.ResourceVersion != before.ResourceVersion {
				t.Fatal("guarded Machine was written to")
			}
			if IsMarkedForRemediation(after) || IsMarkedForRemediation(m) {
				t.Fatal("guarded Machine was marked")
			}
			assertNoEvent(t, rec)
		})
	}
}

func TestMarkForRemediationDryRunWritesNothing(t *testing.T) {
	m := newMachine("w")
	a, rec := newActuator(t, m)
	a.DryRun = true
	before := stored(t, a.Client, m)

	skip, err := a.MarkForRemediation(context.Background(), m, testReason)
	if err != nil {
		t.Fatalf("MarkForRemediation: %v", err)
	}
	if skip != "" {
		t.Fatalf("dry run skipped with %q, want the would-mark result", skip)
	}

	after := stored(t, a.Client, m)
	if after.ResourceVersion != before.ResourceVersion || IsMarkedForRemediation(after) || IsMarkedForRemediation(m) {
		t.Fatal("dry run wrote to the Machine")
	}
	assertNoEvent(t, rec)

	// The guards still apply, so a dry run explains what a live run would skip.
	if skip, _ := a.MarkForRemediation(context.Background(), newMachine("cp", controlPlane), testReason); skip != SkipControlPlane {
		t.Fatalf("dry run on a control plane Machine = %q, want %q", skip, SkipControlPlane)
	}
	assertNoEvent(t, rec)
}

func TestMarkForRemediationReportsPatchFailure(t *testing.T) {
	// The Machine is not in the fake cluster, so the patch fails with NotFound
	// just as it would once a Machine has been deleted under us.
	m := newMachine("gone")
	a, rec := newActuator(t)

	skip, err := a.MarkForRemediation(context.Background(), m, testReason)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want NotFound", err)
	}
	if skip != "" {
		t.Fatalf("skip = %q on a failed patch", skip)
	}
	if !strings.Contains(err.Error(), "default/gone") {
		t.Fatalf("error does not name the Machine: %v", err)
	}
	// A failed patch must not leave the caller's copy claiming success.
	if IsMarkedForRemediation(m) {
		t.Fatal("in-memory Machine was marked although the patch failed")
	}
	assertNoEvent(t, rec)
}

func TestEventNote(t *testing.T) {
	short := strings.Repeat("a", EventNoteLimit)
	if got := EventNote(short); got != short {
		t.Fatal("a note at the limit was changed")
	}

	// The cut lands inside a multi-byte rune, which must not be split.
	long := strings.Repeat("a", EventNoteLimit-4) + "한글"
	got := EventNote(long)
	if len(got) > EventNoteLimit || !strings.HasSuffix(got, "...") || !utf8.ValidString(got) {
		t.Fatalf("EventNote() = %d bytes, valid %v, suffix %q", len(got), utf8.ValidString(got), got[len(got)-3:])
	}
}

func TestMarkForRemediationKeepsEventNotesWithinTheLimit(t *testing.T) {
	m := newMachine("w")
	a, rec := newActuator(t, m)

	if _, err := a.MarkForRemediation(context.Background(), m, strings.Repeat("x", 2*EventNoteLimit)); err != nil {
		t.Fatalf("MarkForRemediation: %v", err)
	}
	e := <-rec.Events
	if note := strings.TrimPrefix(e, "Warning MarkedForRemediation "); len(note) > EventNoteLimit {
		t.Fatalf("event note is %d bytes", len(note))
	}
}

func TestRecordSkipped(t *testing.T) {
	m := newMachine("cp", controlPlane)
	a, rec := newActuator(t, m)

	a.RecordSkipped(m, SkipControlPlane, testReason)
	assertEvent(t, rec, "Warning RemediationSkipped Remediation skipped because the Machine is a control plane member: "+testReason)

	a.RecordSkipped(m, SkipPaused, testReason)
	assertEvent(t, rec, "Warning RemediationSkipped Remediation skipped because the Machine is paused: "+testReason)

	// Remediation under way is not a dropped fault, so it is not recorded
	// on every poll.
	a.RecordSkipped(m, SkipAlreadyMarked, testReason)
	a.RecordSkipped(m, SkipDeleting, testReason)
	assertNoEvent(t, rec)

	// Nothing was skipped, so there is nothing to record.
	a.RecordSkipped(m, "", testReason)
	assertNoEvent(t, rec)

	a.DryRun = true
	a.RecordSkipped(m, SkipControlPlane, testReason)
	assertNoEvent(t, rec)
}
