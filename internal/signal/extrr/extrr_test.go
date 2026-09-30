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
	"errors"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal"
)

// request builds a request the way NVSentinel's janitor leaves it once it
// has released the node: NVSentinelOwnershipReleased True and the answer
// still Unknown.
func request(name string, event map[string]any, mutate ...func(*unstructured.Unstructured)) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"healthEvent": event},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": ConditionOwnershipReleased, "status": "True", "reason": "ReleaseTaintApplied"},
			map[string]any{"type": ConditionComplete, "status": "Unknown", "reason": "AwaitingExternalSystem"},
		}},
	}}
	obj.SetGroupVersionKind(GroupVersionKind)
	obj.SetName(name)
	for _, f := range mutate {
		f(obj)
	}

	return obj
}

// withCondition sets the status of one condition type.
func withCondition(conditionType, status string) func(*unstructured.Unstructured) {
	return func(obj *unstructured.Unstructured) {
		conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		for _, c := range conditions {
			if condition := c.(map[string]any); condition["type"] == conditionType {
				condition["status"] = status
			}
		}
		_ = unstructured.SetNestedSlice(obj.Object, conditions, "status", "conditions")
	}
}

// xid79 is a health event as fault remediation renders it for an XID 79.
func xid79() map[string]any {
	return map[string]any{
		"nodeName":          "gpu-w-1",
		"checkName":         "SysLogsXIDError",
		"id":                "68f0c0ffee",
		"recommendedAction": "RESTART_BM",
		"errorCode":         []any{"79"},
		"entitiesImpacted": []any{
			map[string]any{"entityType": "PCI", "entityValue": "0000:ff:00.0"},
			map[string]any{"entityType": "GPU_UUID", "entityValue": "GPU-1234"},
		},
		"message": "GPU has fallen off the bus",
	}
}

func TestParse(t *testing.T) {
	sig, ok := Parse(request("extrr-68f0c0ffee", xid79()))
	if !ok {
		t.Fatal("Parse rejected a request with a node")
	}

	want := signal.Signal{
		Origin:     signal.OriginExternalRemediationRequest,
		Node:       "gpu-w-1",
		Check:      "SysLogsXIDError",
		ID:         "68f0c0ffee",
		Actions:    []string{signal.ActionRestartBM},
		ErrorCodes: []string{"79"},
		GpuUUIDs:   []string{"GPU-1234"},
		Request:    "extrr-68f0c0ffee",
	}
	if !reflect.DeepEqual(sig, want) {
		t.Fatalf("Parse() = %+v, want %+v", sig, want)
	}
}

func TestParseRecommendedAction(t *testing.T) {
	tests := []struct {
		name   string
		action any
		custom string
		want   []string
	}{
		{"name", "REPLACE_VM", "", []string{signal.ActionReplaceVM}},
		// The CRD accepts the enum's number too, and the JSON decoder hands
		// numbers over as int64 or float64.
		{"number", int64(24), "", []string{signal.ActionRestartBM}},
		{"number decoded as float", float64(25), "", []string{signal.ActionReplaceVM}},
		{"fractional number", float64(24.5), "", []string{signal.ActionUnknown}},
		{"number outside the enum", int64(7), "", []string{signal.ActionUnknown}},
		{"custom by name", "CUSTOM", "drain-and-reimage", []string{"drain-and-reimage"}},
		{"custom by number", int64(27), "drain-and-reimage", []string{"drain-and-reimage"}},
		{"custom without a name", "CUSTOM", "", []string{signal.ActionCustom}},
		// The custom name only counts for CUSTOM.
		{"custom name on a built-in action", "RESTART_VM", "drain-and-reimage", []string{signal.ActionRestartVM}},
		{"missing", nil, "", nil},
		{"empty", "", "", nil},
		{"wrong type", true, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := map[string]any{"nodeName": "gpu-w-1", "checkName": "c"}
			if tt.action != nil {
				event["recommendedAction"] = tt.action
			}
			if tt.custom != "" {
				event["customRecommendedAction"] = tt.custom
			}

			sig, ok := Parse(request("r", event))
			if !ok {
				t.Fatal("Parse rejected the request")
			}
			if !reflect.DeepEqual(sig.Actions, tt.want) {
				t.Fatalf("Actions = %q, want %q", sig.Actions, tt.want)
			}
		})
	}
}

func TestParseFallsBackToTheRequestName(t *testing.T) {
	sig, ok := Parse(request("extrr-1", map[string]any{"nodeName": "gpu-w-1", "recommendedAction": "RESTART_VM"}))
	if !ok || sig.Check != "extrr-1" {
		t.Fatalf("Parse() = %+v, %v; want the request name as the check", sig, ok)
	}
}

func TestParseIgnoresMalformedEntities(t *testing.T) {
	event := xid79()
	event["entitiesImpacted"] = []any{
		"not an entity",
		map[string]any{"entityType": "GPU_UUID"},
		map[string]any{"entityType": "GPU_UUID", "entityValue": int64(7)},
		map[string]any{"entityType": "GPU_UUID", "entityValue": "GPU-5678"},
	}

	sig, _ := Parse(request("r", event))
	if !reflect.DeepEqual(sig.GpuUUIDs, []string{"GPU-5678"}) {
		t.Fatalf("GpuUUIDs = %q, want only the well-formed one", sig.GpuUUIDs)
	}
}

func TestParseRejectsRequestsWithoutNode(t *testing.T) {
	for name, obj := range map[string]*unstructured.Unstructured{
		"no event":   {Object: map[string]any{}},
		"no node":    request("r", map[string]any{"recommendedAction": "RESTART_VM"}),
		"empty node": request("r", map[string]any{"nodeName": "", "recommendedAction": "RESTART_VM"}),
	} {
		if _, ok := Parse(obj); ok {
			t.Errorf("%s: Parse accepted a request without a node", name)
		}
	}
}

func TestPending(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name   string
		mutate func(*unstructured.Unstructured)
		want   bool
	}{
		{"released and awaiting an answer", func(*unstructured.Unstructured) {}, true},
		// Another request holds the node, or the janitor has not got to it.
		{"not released yet", withCondition(ConditionOwnershipReleased, "Unknown"), false},
		{"node not found", withCondition(ConditionOwnershipReleased, "False"), false},
		{"no status yet", func(obj *unstructured.Unstructured) { delete(obj.Object, "status") }, false},
		{"answered True", withCondition(ConditionComplete, "True"), false},
		{"answered False", withCondition(ConditionComplete, "False"), false},
		{"being deleted", func(obj *unstructured.Unstructured) { obj.SetDeletionTimestamp(&now) }, false},
		{"malformed conditions", func(obj *unstructured.Unstructured) {
			obj.Object["status"] = map[string]any{"conditions": []any{"garbage", map[string]any{"type": ConditionOwnershipReleased, "status": int64(1)}}}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Pending(request("r", xid79(), tt.mutate)); got != tt.want {
				t.Fatalf("Pending() = %v, want %v", got, tt.want)
			}
		})
	}
}

// newReader returns a fake workload cluster serving the requests as the
// cluster-scoped resource they are.
func newReader(t *testing.T, funcs interceptor.Funcs, objs ...*unstructured.Unstructured) client.Client {
	t.Helper()

	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(GroupVersionKind, meta.RESTScopeRoot)

	built := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		built = append(built, o)
	}

	return fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithRESTMapper(mapper).
		WithObjects(built...).WithInterceptorFuncs(funcs).Build()
}

func TestSourceCollectsPendingRequests(t *testing.T) {
	other := xid79()
	other["nodeName"] = "gpu-w-0"
	other["recommendedAction"] = "REPLACE_VM"

	gpuCheck := xid79()
	gpuCheck["checkName"] = "GpuXidError"

	src := NewSource(newReader(t, interceptor.Funcs{},
		// NVSentinel releases a node to one request at a time, so these
		// three on gpu-w-1 cannot occur; they are here only to pin down the
		// sort order by check and then by request.
		request("extrr-b", xid79()),
		request("extrr-a2", xid79()),
		request("extrr-c", gpuCheck),
		request("extrr-a", other),
		request("extrr-done", xid79(), withCondition(ConditionComplete, "True")),
		request("extrr-waiting", xid79(), withCondition(ConditionOwnershipReleased, "Unknown")),
		request("extrr-nodeless", map[string]any{"recommendedAction": "RESTART_VM"}),
	))
	if src.Name() != SourceName {
		t.Fatalf("Name() = %q", src.Name())
	}

	signals, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	var got []string
	for _, s := range signals {
		got = append(got, s.Request)
	}
	if want := []string{"extrr-a", "extrr-c", "extrr-a2", "extrr-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %q, want %q sorted by node, check and request", got, want)
	}
}

func TestSourceCollectReportsListFailure(t *testing.T) {
	boom := errors.New("boom")
	src := NewSource(newReader(t, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return boom },
	}))

	if _, err := src.Collect(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Collect error = %v, want it to wrap %v", err, boom)
	}
}
