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

package quarantine

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	ns      = "tenant-a"
	vmName  = "gpu-np-abc12"
	vmiUID  = "vmi-uid-1"
	podUID  = "pod-uid-1"
	claim   = "virt-launcher-gpu-np-abc12-devices-x7k2p"
	cluster = "gpu"
)

var (
	kubevirtMachineGVK = kubevirtMachineKind.WithVersion("v1alpha1")
	kubevirtClusterGVK = kubevirtClusterKind.WithVersion("v1alpha1")
	vmiGVK             = vmiKind.WithVersion("v1")
	ruleV1GVK          = deviceTaintRuleKind.WithVersion("v1")
	ruleV1beta2GVK     = deviceTaintRuleKind.WithVersion("v1beta2")
)

// newMapper returns a mapper serving the built-in kinds the tests use and
// the given extra kinds, the way discovery would.
func newMapper(extra ...schema.GroupVersionKind) meta.RESTMapper {
	namespaced := []schema.GroupVersionKind{
		corev1.SchemeGroupVersion.WithKind("Pod"),
		resourcev1.SchemeGroupVersion.WithKind("ResourceClaim"),
		clusterv1.GroupVersion.WithKind("Machine"),
		clusterv1.GroupVersion.WithKind("Cluster"),
	}
	var gvs []schema.GroupVersion
	for _, gvk := range append(slices.Clone(namespaced), extra...) {
		if !slices.Contains(gvs, gvk.GroupVersion()) {
			gvs = append(gvs, gvk.GroupVersion())
		}
	}
	m := meta.NewDefaultRESTMapper(gvs)
	for _, gvk := range namespaced {
		m.Add(gvk, meta.RESTScopeNamespace)
	}
	for _, gvk := range extra {
		scope := meta.RESTScopeNamespace
		if gvk.GroupKind() == deviceTaintRuleKind {
			scope = meta.RESTScopeRoot
		}
		m.Add(gvk, scope)
	}

	return m
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := clusterv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}

	return s
}

func newObject(gvk schema.GroupVersionKind, namespace, name string, fields map[string]any) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: fields}
	if o.Object == nil {
		o.Object = map[string]any{}
	}
	o.SetGroupVersionKind(gvk)
	o.SetNamespace(namespace)
	o.SetName(name)

	return o
}

func kubevirtMachine(fields map[string]any) *unstructured.Unstructured {
	return newObject(kubevirtMachineGVK, ns, vmName, fields)
}

func kubevirtCluster(fields map[string]any) *unstructured.Unstructured {
	return newObject(kubevirtClusterGVK, ns, cluster, fields)
}

// vmi is a running VMI, listing the given pods as active.
func vmi(namespace string, activePods map[string]any) *unstructured.Unstructured {
	o := newObject(vmiGVK, namespace, vmName, nil)
	o.SetUID(vmiUID)
	status := map[string]any{"phase": vmiRunning}
	if activePods != nil {
		status["activePods"] = activePods
	}
	o.Object["status"] = status

	return o
}

func launcher(namespace, name, uid string, phase corev1.PodPhase, claims ...string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name, UID: types.UID(uid),
			Labels: map[string]string{createdByLabel: vmiUID},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
	// Claims made from a template, which only the status names.
	for i := range claims {
		ref := "devices-" + strconv.Itoa(i)
		p.Spec.ResourceClaims = append(p.Spec.ResourceClaims, corev1.PodResourceClaim{Name: ref, ResourceClaimTemplateName: ptr("devices")})
		p.Status.ResourceClaimStatuses = append(p.Status.ResourceClaimStatuses, corev1.PodResourceClaimStatus{Name: ref, ResourceClaimName: &claims[i]})
	}

	return p
}

func resourceClaim(namespace, name string, results ...resourcev1.DeviceRequestAllocationResult) *resourcev1.ResourceClaim {
	c := &resourcev1.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	if results != nil {
		c.Status.Allocation = &resourcev1.AllocationResult{Devices: resourcev1.DeviceAllocationResult{Results: results}}
	}

	return c
}

func result(d Device) resourcev1.DeviceRequestAllocationResult {
	return resourcev1.DeviceRequestAllocationResult{Request: "devices", Driver: d.Driver, Pool: d.Pool, Device: d.Name}
}

func testMachine() *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gpu-np-abc12-m"},
		Spec: clusterv1.MachineSpec{
			ClusterName: cluster,
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: kubevirtMachineKind.Group, Kind: kubevirtMachineKind.Kind, Name: vmName,
			},
		},
	}
}

func testCluster() *clusterv1.Cluster {
	return &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: cluster},
		Spec: clusterv1.ClusterSpec{
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: kubevirtClusterKind.Group, Kind: kubevirtClusterKind.Kind, Name: cluster,
			},
		},
	}
}

func ptr[T any](v T) *T { return &v }

func newLocator(t *testing.T, mapper meta.RESTMapper, objs ...client.Object) *KubeVirtLocator {
	t.Helper()

	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithRESTMapper(mapper).WithObjects(objs...).Build()

	return &KubeVirtLocator{Client: c, Mapper: mapper}
}

func TestKubeVirtLocator(t *testing.T) {
	kubevirt := newMapper(kubevirtMachineGVK, kubevirtClusterGVK, vmiGVK)
	admin := result(gpu1)
	admin.AdminAccess = ptr(true)
	other := Device{Driver: gpuDriver, Pool: "node-b", Name: "gpu-9"}

	cases := []struct {
		name    string
		mapper  meta.RESTMapper
		objs    []client.Object
		machine func(*clusterv1.Machine)
		want    []Device
		reason  Reason
	}{
		{
			name: "the devices of the launcher's claims",
			objs: []client.Object{
				kubevirtMachine(nil), kubevirtCluster(nil), vmi(ns, nil),
				launcher(ns, "virt-launcher-a", podUID, corev1.PodRunning, claim),
				resourceClaim(ns, claim, result(gpu0), result(nic0), admin),
			},
			want: []Device{gpu0, nic0},
		},
		{
			name: "a claim the pod names itself",
			objs: []client.Object{
				kubevirtMachine(nil), kubevirtCluster(nil), vmi(ns, nil),
				func() *corev1.Pod {
					p := launcher(ns, "virt-launcher-a", podUID, corev1.PodRunning)
					p.Spec.ResourceClaims = []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: ptr("shared-gpu")}}
					return p
				}(),
				resourceClaim(ns, "shared-gpu", result(gpu1)),
			},
			want: []Device{gpu1},
		},
		{
			name: "the VM in its template's namespace",
			objs: []client.Object{
				kubevirtMachine(map[string]any{"spec": map[string]any{"virtualMachineTemplate": map[string]any{
					"metadata": map[string]any{"namespace": "vms"},
				}}}),
				kubevirtCluster(nil), vmi("vms", nil),
				launcher("vms", "virt-launcher-a", podUID, corev1.PodRunning, claim),
				resourceClaim("vms", claim, result(gpu0)),
				// The same names in the Machine's namespace are not the VM's.
				launcher(ns, "virt-launcher-a", "pod-uid-ns", corev1.PodRunning, claim),
				resourceClaim(ns, claim, result(other)),
			},
			want: []Device{gpu0},
		},
		{
			name: "only the pods the VMI lists as active",
			objs: []client.Object{
				kubevirtMachine(nil), kubevirtCluster(nil), vmi(ns, map[string]any{podUID: "node-a"}),
				launcher(ns, "virt-launcher-a", podUID, corev1.PodRunning, claim),
				launcher(ns, "virt-launcher-b", "pod-uid-2", corev1.PodPending, "other-claim"),
				resourceClaim(ns, claim, result(gpu0)),
				resourceClaim(ns, "other-claim", result(other)),
			},
			want: []Device{gpu0},
		},
		{
			name: "terminated pods and claims gone or unallocated hold nothing",
			objs: []client.Object{
				kubevirtMachine(nil), kubevirtCluster(nil), vmi(ns, nil),
				launcher(ns, "virt-launcher-a", podUID, corev1.PodRunning, claim, "gone", "pending"),
				launcher(ns, "virt-launcher-old", "pod-uid-0", corev1.PodFailed, "old-claim"),
				resourceClaim(ns, claim, result(gpu0)),
				resourceClaim(ns, "pending"),
				resourceClaim(ns, "old-claim", result(other)),
			},
			want: []Device{gpu0},
		},
		{
			name:    "not a KubeVirt Machine",
			objs:    []client.Object{kubevirtMachine(nil)},
			machine: func(m *clusterv1.Machine) { m.Spec.InfrastructureRef.Kind = "Metal3Machine" },
			reason:  ReasonNotKubeVirt,
		},
		{
			name:   "KubeVirt not installed",
			mapper: newMapper(kubevirtMachineGVK, kubevirtClusterGVK),
			objs:   []client.Object{kubevirtMachine(nil), kubevirtCluster(nil)},
			reason: ReasonNotKubeVirt,
		},
		{
			name: "an external infrastructure cluster on the Machine",
			objs: []client.Object{
				kubevirtMachine(map[string]any{"spec": map[string]any{"infraClusterSecretRef": map[string]any{"name": "infra"}}}),
				kubevirtCluster(nil), vmi(ns, nil),
			},
			reason: ReasonExternalInfraCluster,
		},
		{
			name: "an external infrastructure cluster on the Cluster",
			objs: []client.Object{
				kubevirtMachine(nil),
				kubevirtCluster(map[string]any{"spec": map[string]any{"infraClusterSecretRef": map[string]any{"name": "infra"}}}),
				vmi(ns, nil),
			},
			reason: ReasonExternalInfraCluster,
		},
		{
			name:   "no KubevirtMachine",
			objs:   []client.Object{kubevirtCluster(nil)},
			reason: ReasonNoDevices,
		},
		{
			name:   "the VM is not running",
			objs:   []client.Object{kubevirtMachine(nil), kubevirtCluster(nil)},
			reason: ReasonNoDevices,
		},
		{
			name: "a VM that is starting",
			objs: []client.Object{
				kubevirtMachine(nil), kubevirtCluster(nil),
				func() *unstructured.Unstructured {
					o := vmi(ns, nil)
					o.Object["status"] = map[string]any{"phase": "Scheduling"}
					return o
				}(),
				launcher(ns, "virt-launcher-a", podUID, corev1.PodPending, claim),
				resourceClaim(ns, claim, result(gpu0)),
			},
			reason: ReasonNoDevices,
		},
		{
			name: "a VM that is being deleted",
			objs: []client.Object{
				kubevirtMachine(nil), kubevirtCluster(nil),
				func() *unstructured.Unstructured {
					o := vmi(ns, nil)
					o.SetFinalizers([]string{"kubevirt.io/virtualMachineControllerFinalize"})
					o.SetDeletionTimestamp(&metav1.Time{Time: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)})
					return o
				}(),
				launcher(ns, "virt-launcher-a", podUID, corev1.PodRunning, claim),
				resourceClaim(ns, claim, result(gpu0)),
			},
			reason: ReasonNoDevices,
		},
		{
			name: "a VM with no launcher holds nothing",
			objs: []client.Object{kubevirtMachine(nil), kubevirtCluster(nil), vmi(ns, nil)},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mapper := c.mapper
			if mapper == nil {
				mapper = kubevirt
			}
			m := testMachine()
			if c.machine != nil {
				c.machine(m)
			}

			got, err := newLocator(t, mapper, c.objs...).Devices(context.Background(), testCluster(), m)
			if c.reason != "" {
				if reasonOf(err) != c.reason {
					t.Fatalf("Devices = %v, %v; want a %s rejection", got, err, c.reason)
				}
				return
			}
			if err != nil || !slices.Equal(got, c.want) {
				t.Errorf("Devices = %v, %v; want %v", got, err, c.want)
			}
		})
	}
}
