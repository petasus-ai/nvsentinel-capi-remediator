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

package extrr

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// newStatusClient returns a fake workload cluster serving the requests with
// their status subresource.
func newStatusClient(t *testing.T, funcs interceptor.Funcs, objs ...*unstructured.Unstructured) client.Client {
	t.Helper()

	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(GroupVersionKind, meta.RESTScopeRoot)

	built := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		built = append(built, o)
	}

	return fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithRESTMapper(mapper).
		WithObjects(built...).WithStatusSubresource(built...).WithInterceptorFuncs(funcs).Build()
}

func stored(t *testing.T, c client.Client, name string) *unstructured.Unstructured {
	t.Helper()

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(GroupVersionKind)
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, obj); err != nil {
		t.Fatalf("get request: %v", err)
	}

	return obj
}

func condition(t *testing.T, obj *unstructured.Unstructured, conditionType string) map[string]any {
	t.Helper()

	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conditions {
		if cond := c.(map[string]any); cond["type"] == conditionType {
			return cond
		}
	}
	t.Fatalf("no %s condition in %v", conditionType, conditions)

	return nil
}

func TestAnswer(t *testing.T) {
	for _, complete := range []bool{true, false} {
		c := newStatusClient(t, interceptor.Funcs{}, request("extrr-1", xid79()))

		answered, err := Answer(context.Background(), c, "extrr-1", complete, "Restarted", "node gpu-w-1 restarted")
		if err != nil || !answered {
			t.Fatalf("Answer(%v) = %v, %v; want answered", complete, answered, err)
		}

		obj := stored(t, c, "extrr-1")
		want := "False"
		if complete {
			want = "True"
		}
		got := condition(t, obj, ConditionComplete)
		if got["status"] != want || got["reason"] != "Restarted" || got["message"] != "node gpu-w-1 restarted" {
			t.Fatalf("answer = %v, want status %s with the reason and message", got, want)
		}
		if _, err := time.Parse(time.RFC3339, got["lastTransitionTime"].(string)); err != nil {
			t.Fatalf("lastTransitionTime: %v", err)
		}
		if _, ok := got["observedGeneration"].(int64); !ok {
			t.Fatalf("observedGeneration = %#v, want an integer", got["observedGeneration"])
		}
		// NVSentinel's own condition is kept as it was.
		if released := condition(t, obj, ConditionOwnershipReleased); released["status"] != "True" || released["reason"] != "ReleaseTaintApplied" {
			t.Fatalf("released condition changed: %v", released)
		}
		if Pending(obj) {
			t.Fatal("an answered request is still pending")
		}
	}
}

func TestAnswerAddsTheConditionWhenMissing(t *testing.T) {
	obj := request("extrr-1", xid79())
	_ = unstructured.SetNestedSlice(obj.Object, []any{
		map[string]any{"type": ConditionOwnershipReleased, "status": "True"},
	}, "status", "conditions")
	c := newStatusClient(t, interceptor.Funcs{}, obj)

	if answered, err := Answer(context.Background(), c, "extrr-1", true, "Restarted", "m"); err != nil || !answered {
		t.Fatalf("Answer = %v, %v", answered, err)
	}
	if got := condition(t, stored(t, c, "extrr-1"), ConditionComplete); got["status"] != "True" {
		t.Fatalf("answer = %v", got)
	}
}

func TestAnswerLeavesAnsweredAndMissingRequestsAlone(t *testing.T) {
	c := newStatusClient(t, interceptor.Funcs{
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			t.Fatal("patched a request that needs no answer")
			return nil
		},
	}, request("extrr-done", xid79(), withCondition(ConditionComplete, "False")))

	for _, name := range []string{"extrr-done", "extrr-gone"} {
		if answered, err := Answer(context.Background(), c, name, true, "Restarted", "m"); err != nil || answered {
			t.Fatalf("Answer(%s) = %v, %v; want nothing done", name, answered, err)
		}
	}
	if got := condition(t, stored(t, c, "extrr-done"), ConditionComplete); got["status"] != "False" {
		t.Fatalf("an earlier answer was changed: %v", got)
	}
}

func TestAnswerLeavesRequestsThatAreNotPendingAlone(t *testing.T) {
	now := metav1.Now()
	c := newStatusClient(t, interceptor.Funcs{
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			t.Fatal("patched a request that is not pending")
			return nil
		},
	},
		request("extrr-waiting", xid79(), withCondition(ConditionOwnershipReleased, "Unknown")),
		request("extrr-deleting", xid79(), func(obj *unstructured.Unstructured) {
			obj.SetDeletionTimestamp(&now)
			obj.SetFinalizers([]string{"nvsentinel.dgxc.nvidia.com/external-remediation-cleanup"})
		}),
		request("extrr-malformed", xid79(), func(obj *unstructured.Unstructured) { obj.Object["status"] = "garbage" }),
	)

	for _, name := range []string{"extrr-waiting", "extrr-deleting", "extrr-malformed"} {
		if answered, err := Answer(context.Background(), c, name, true, "Restarted", "m"); err != nil || answered {
			t.Errorf("Answer(%s) = %v, %v; want nothing done", name, answered, err)
		}
	}
}

func TestIsPending(t *testing.T) {
	c := newStatusClient(t, interceptor.Funcs{},
		request("extrr-pending", xid79()),
		request("extrr-done", xid79(), withCondition(ConditionComplete, "True")))

	for name, want := range map[string]bool{"extrr-pending": true, "extrr-done": false, "extrr-gone": false} {
		if got, err := IsPending(context.Background(), c, name); err != nil || got != want {
			t.Errorf("IsPending(%s) = %v, %v; want %v", name, got, err, want)
		}
	}

	boom := errors.New("boom")
	failing := newStatusClient(t, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return boom
		},
	})
	if _, err := IsPending(context.Background(), failing, "extrr-pending"); !errors.Is(err, boom) {
		t.Fatalf("IsPending error = %v, want it to wrap %v", err, boom)
	}
}

func TestAnswerReportsFailures(t *testing.T) {
	boom := errors.New("boom")
	conflict := apierrors.NewConflict(schema.GroupResource{Group: GroupVersionKind.Group, Resource: "externalremediationrequests"}, "extrr-1", boom)

	tests := map[string]interceptor.Funcs{
		"get": {Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return boom
		}},
		"conflicting patch": {SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return conflict
		}},
	}
	for name, funcs := range tests {
		t.Run(name, func(t *testing.T) {
			c := newStatusClient(t, funcs, request("extrr-1", xid79()))
			answered, err := Answer(context.Background(), c, "extrr-1", true, "Restarted", "m")
			if err == nil || answered {
				t.Fatalf("Answer = %v, %v; want an error", answered, err)
			}
		})
	}
}

func TestAnswerPatchesUnderTheResourceVersion(t *testing.T) {
	// The API server refuses a patch carrying a resource version that is
	// no longer current, which keeps an answer from undoing a change
	// NVSentinel made to the conditions in between. The fake client does
	// not enforce that for subresources, so the patch itself is checked.
	var body map[string]any
	c := newStatusClient(t, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			data, err := patch.Data(obj)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(data, &body); err != nil {
				return err
			}
			if sub != "status" {
				t.Errorf("patched subresource %q, want status", sub)
			}
			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}, request("extrr-1", xid79()))
	current := stored(t, c, "extrr-1").GetResourceVersion()

	if answered, err := Answer(context.Background(), c, "extrr-1", true, "Restarted", "m"); err != nil || !answered {
		t.Fatalf("Answer = %v, %v", answered, err)
	}

	if rv, _, _ := unstructured.NestedString(body, "metadata", "resourceVersion"); rv != current {
		t.Fatalf("patch resourceVersion = %q, want %q", rv, current)
	}
	delete(body, "metadata")
	status, _ := body["status"].(map[string]any)
	if len(body) != 1 || len(status) != 1 || status["conditions"] == nil {
		t.Fatalf("patch = %v, want only the conditions", body)
	}
}
