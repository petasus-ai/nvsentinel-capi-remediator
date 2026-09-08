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

package condition

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal"
)

func node(name string, conds ...corev1.NodeCondition) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Conditions: conds},
	}
}

func cond(typ, message string, status corev1.ConditionStatus) corev1.NodeCondition {
	return corev1.NodeCondition{Type: corev1.NodeConditionType(typ), Status: status, Message: message}
}

func newReader(t *testing.T, funcs interceptor.Funcs, nodes ...*corev1.Node) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	objs := make([]client.Object, 0, len(nodes))
	for _, n := range nodes {
		objs = append(objs, n)
	}

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
}

func TestSourceCollectsOnlyRaisedNVSentinelConditions(t *testing.T) {
	reader := newReader(t, interceptor.Funcs{},
		node("gpu-w-2",
			cond("GpuXidError", "ErrorCode:DCGM_FR_XID_ERROR GPU_UUID:GPU-2222 xid 79 seen Recommended Action=REPLACE_VM;", corev1.ConditionTrue),
			cond("GpuThermal", "ErrorCode:DCGM_FR_CLOCK_THROTTLE_THERMAL Recommended Action=CONTACT_SUPPORT;", corev1.ConditionTrue),
			// Healthy: a False NVSentinel condition and a True Kubernetes one.
			cond("GpuNvlinkError", "ErrorCode:DCGM_FR_NVLINK_ERROR Recommended Action=RESTART_VM;", corev1.ConditionFalse),
			cond("Ready", "kubelet is posting ready status", corev1.ConditionTrue),
		),
		node("gpu-w-1",
			cond("GpuFabricError", "ErrorCode:GPU_FABRIC_DEGRADED Recommended Action=RESTART_BM;", corev1.ConditionTrue),
		),
		node("cp-1", cond("Ready", "", corev1.ConditionTrue)),
	)

	src := NewSource(reader)
	if src.Name() != SourceName {
		t.Fatalf("Name() = %q", src.Name())
	}

	got, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	want := []signal.Signal{
		{Origin: signal.OriginNodeCondition, Node: "gpu-w-1", Check: "GpuFabricError",
			Actions: []string{"RESTART_BM"}, ErrorCodes: []string{"GPU_FABRIC_DEGRADED"}},
		{Origin: signal.OriginNodeCondition, Node: "gpu-w-2", Check: "GpuThermal",
			Actions: []string{"CONTACT_SUPPORT"}, ErrorCodes: []string{"DCGM_FR_CLOCK_THROTTLE_THERMAL"}},
		{Origin: signal.OriginNodeCondition, Node: "gpu-w-2", Check: "GpuXidError",
			Actions: []string{"REPLACE_VM"}, ErrorCodes: []string{"DCGM_FR_XID_ERROR"}, GpuUUIDs: []string{"GPU-2222"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Collect() = %+v\nwant %+v", got, want)
	}
}

func TestSourceCollectReturnsNothingForHealthyClusters(t *testing.T) {
	reader := newReader(t, interceptor.Funcs{}, node("w", cond("Ready", "", corev1.ConditionTrue)))

	got, err := NewSource(reader).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Collect() = %+v, want none", got)
	}
}

func TestSourceCollectReportsListFailures(t *testing.T) {
	boom := errors.New("apiserver unreachable")
	reader := newReader(t, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return boom
		},
	})

	_, err := NewSource(reader).Collect(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("Collect error = %v, want it to wrap %v", err, boom)
	}
}
