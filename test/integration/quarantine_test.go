//go:build integration

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

package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/controller"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/quarantine"
)

const (
	kvNamespace = "tenant-kv"
	kvCluster   = "kv"
	kvMachine   = "kv-gpu-w-1"
	gpuDriver   = "gpu.example.com"
	nicDriver   = "nic.example.com"
	pool        = "node-a"
	launcherPod = "virt-launcher-kv-gpu-w-1-abcde"
	// keepFinalizer holds the deleted Machine back.
	keepFinalizer = "test.example.com/keep"
	vmClaim       = "virt-launcher-kv-gpu-w-1-devices-x7k2p"
)

var ruleV1beta2 = schema.GroupVersionKind{Group: "resource.k8s.io", Version: "v1beta2", Kind: "DeviceTaintRule"}

// TestDeviceQuarantine replaces a KubeVirt Machine whose VM holds two GPUs
// and a NIC: the request is rejected until it names the GPU, and then the
// GPU is quarantined in the version the API server serves and the Machine
// is deleted.
func TestDeviceQuarantine(t *testing.T) {
	ctx := context.Background()
	createKubeVirtMachine(ctx, t)

	gpu1 := quarantine.Device{Driver: gpuDriver, Pool: pool, Name: "gpu-1"}

	step := func(name string, f func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, f) {
			t.FailNow()
		}
	}

	step("a request that names no device is rejected while there are two", func(t *testing.T) {
		annotate(ctx, t, quarantine.RequestQuarantine, false)

		eventually(t, "the request to be rejected", func() (bool, string) {
			status := kvMachineStatus(ctx, t)
			return status == quarantine.RejectedStatus(quarantine.ReasonAmbiguousDevices), status
		})
		eventually(t, "an Event about the rejection", func() (bool, string) {
			n := countEventsIn(ctx, t, kvNamespace, "Machine", kvMachine, controller.EventQuarantineRejected)
			return n == 1, fmt.Sprintf("%d Events", n)
		})
		if rules := listRules(ctx, t); len(rules) > 0 {
			t.Errorf("a rejected request made rules %v", rules)
		}
	})

	step("the request naming the device quarantines it and replaces the Machine", func(t *testing.T) {
		annotate(ctx, t, "quarantine:gpu-1", true)

		eventually(t, "the Machine to be deleted", func() (bool, string) {
			m := &clusterv1.Machine{}
			must(t, hub.Get(ctx, client.ObjectKey{Namespace: kvNamespace, Name: kvMachine}, m))
			return !m.DeletionTimestamp.IsZero() && m.Annotations[quarantine.ReplaceStatusAnnotation] == quarantine.StatusReplacing,
				fmt.Sprint(m.DeletionTimestamp, m.Annotations)
		})

		rules := listRules(ctx, t)
		if len(rules) != 1 || rules[0].GetName() != quarantine.RuleName(gpu1) {
			t.Fatalf("rules = %v; want the one of %s", rules, gpu1)
		}
		rule := rules[0]
		selector, _, _ := unstructured.NestedStringMap(rule.Object, "spec", "deviceSelector")
		if selector["driver"] != gpuDriver || selector["pool"] != pool || selector["device"] != "gpu-1" {
			t.Errorf("deviceSelector = %v; want %s", selector, gpu1)
		}
		effect, _, _ := unstructured.NestedString(rule.Object, "spec", "taint", "effect")
		if effect != "NoSchedule" {
			t.Errorf("taint effect = %q; want NoSchedule", effect)
		}
		// The API server stamps the time the taint was added.
		if added, _, _ := unstructured.NestedString(rule.Object, "spec", "taint", "timeAdded"); added == "" {
			t.Error("the taint has no timeAdded")
		}
		if got := rule.GetLabels()[quarantine.MachineLabel]; got != kvMachine {
			t.Errorf("the rule's Machine label = %q; want %q", got, kvMachine)
		}

		for _, reason := range []string{controller.EventDevicesQuarantined, controller.EventMachineReplaced} {
			eventually(t, "a "+reason+" Event", func() (bool, string) {
				n := countEventsIn(ctx, t, kvNamespace, "Machine", kvMachine, reason)
				return n == 1, fmt.Sprintf("%d Events", n)
			})
		}
		// The status patches wake the controller again, on a cache that may
		// lag behind; nothing is recorded twice.
		consistently(t, "one Event of each", func() (bool, string) {
			q := countEventsIn(ctx, t, kvNamespace, "Machine", kvMachine, controller.EventDevicesQuarantined)
			r := countEventsIn(ctx, t, kvNamespace, "Machine", kvMachine, controller.EventMachineReplaced)
			return q == 1 && r == 1, fmt.Sprintf("%d %s, %d %s", q, controller.EventDevicesQuarantined, r, controller.EventMachineReplaced)
		})

		// Let the Machine go.
		m := &clusterv1.Machine{}
		must(t, hub.Get(ctx, client.ObjectKey{Namespace: kvNamespace, Name: kvMachine}, m))
		m.Finalizers = nil
		must(t, hub.Update(ctx, m))
	})

	step("a cluster that serves no DeviceTaintRule is told apart", func(t *testing.T) {
		// The workload API server serves resource.k8s.io/v1, but no
		// DeviceTaintRule in it, and no v1beta2 of them at all.
		httpClient, err := rest.HTTPClientFor(workloadCfg)
		must(t, err)
		mapper, err := apiutil.NewDynamicRESTMapper(workloadCfg, httpClient)
		must(t, err)
		rules := &quarantine.Rules{Client: workload, Reader: workload, Mapper: mapper}

		_, err = rules.Exists(ctx, gpu1)
		var rejection *quarantine.Rejection
		if !errors.As(err, &rejection) || rejection.Reason != quarantine.ReasonDeviceTaintRuleUnavailable {
			t.Errorf("Exists = %v; want a %s rejection", err, quarantine.ReasonDeviceTaintRuleUnavailable)
		}
	})
}

// createKubeVirtMachine creates what Cluster API Provider KubeVirt and
// KubeVirt leave behind for a running worker: the Cluster and Machine, the
// KubevirtCluster and KubevirtMachine, the VMI, its virt-launcher pod and
// the pod's claim, allocated two GPUs and a NIC.
func createKubeVirtMachine(ctx context.Context, t *testing.T) {
	t.Helper()

	must(t, hub.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: kvNamespace}}))

	must(t, hub.Create(ctx, infraObject("KubevirtCluster", kvCluster)))
	must(t, hub.Create(ctx, &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: kvNamespace, Name: kvCluster},
		Spec: clusterv1.ClusterSpec{
			ClusterNetwork: clusterv1.ClusterNetwork{ServiceDomain: "cluster.local"},
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: clusterv1.GroupVersionInfrastructure.Group, Kind: "KubevirtCluster", Name: kvCluster,
			},
		},
	}))

	must(t, hub.Create(ctx, infraObject("KubevirtMachine", kvMachine)))
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance"})
	vmi.SetNamespace(kvNamespace)
	vmi.SetName(kvMachine)
	// The stand-in CRD has no status subresource, so the VMI is created
	// running.
	vmi.Object["status"] = map[string]any{"phase": "Running"}
	must(t, hub.Create(ctx, vmi))

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: kvNamespace, Name: vmClaim,
			// The API server only takes an allocation for a claim that is
			// protected from deletion.
			Finalizers: []string{"resource.kubernetes.io/delete-protection"},
		},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{
			{Name: "gpu", Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: gpuDriver}},
			{Name: "nic", Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: nicDriver}},
		}}},
	}
	must(t, hub.Create(ctx, claim))
	claim.Status.Allocation = &resourcev1.AllocationResult{Devices: resourcev1.DeviceAllocationResult{Results: []resourcev1.DeviceRequestAllocationResult{
		{Request: "gpu", Driver: gpuDriver, Pool: pool, Device: "gpu-0"},
		{Request: "gpu", Driver: gpuDriver, Pool: pool, Device: "gpu-1"},
		{Request: "nic", Driver: nicDriver, Pool: pool, Device: "nic-0"},
	}}}
	must(t, hub.Status().Update(ctx, claim))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: kvNamespace, Name: launcherPod,
			Labels: map[string]string{"kubevirt.io/created-by": string(vmi.GetUID())},
		},
		Spec: corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "compute", Image: "example.com/virt-launcher"}},
			ResourceClaims: []corev1.PodResourceClaim{{Name: "devices", ResourceClaimTemplateName: ptrTo("devices")}},
		},
	}
	must(t, hub.Create(ctx, pod))
	pod.Status.ResourceClaimStatuses = []corev1.PodResourceClaimStatus{{Name: "devices", ResourceClaimName: ptrTo(vmClaim)}}
	must(t, hub.Status().Update(ctx, pod))

	controllerRef := true
	must(t, hub.Create(ctx, &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: kvNamespace,
			Name:      kvMachine,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: kvCluster},
			// Held back after its deletion, the way Cluster API holds a
			// Machine while it drains the node, so that the controller sees
			// it deleted and must leave it alone.
			Finalizers: []string{keepFinalizer},
			// The MachineSet itself is not needed: nothing collects garbage
			// here.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(), Kind: "MachineSet", Name: "kv-gpu", UID: "machineset-uid",
				Controller: &controllerRef,
			}},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: kvCluster,
			Bootstrap:   clusterv1.Bootstrap{DataSecretName: ptrTo("bootstrap")},
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: clusterv1.GroupVersionInfrastructure.Group, Kind: "KubevirtMachine", Name: kvMachine,
			},
		},
	}))
}

func infraObject(kind, name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}
	o.SetGroupVersionKind(schema.GroupVersionKind{Group: clusterv1.GroupVersionInfrastructure.Group, Version: "v1alpha1", Kind: kind})
	o.SetNamespace(kvNamespace)
	o.SetName(name)

	return o
}

// annotate sets the Machine's replace request, removing the status of the
// last one when asked to, the way an operator tries again.
func annotate(ctx context.Context, t *testing.T, request string, clearStatus bool) {
	t.Helper()

	m := &clusterv1.Machine{}
	must(t, hub.Get(ctx, client.ObjectKey{Namespace: kvNamespace, Name: kvMachine}, m))
	patched := m.DeepCopy()
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	patched.Annotations[quarantine.ReplaceAnnotation] = request
	if clearStatus {
		delete(patched.Annotations, quarantine.ReplaceStatusAnnotation)
	}
	must(t, hub.Patch(ctx, patched, client.MergeFrom(m)))
}

func kvMachineStatus(ctx context.Context, t *testing.T) string {
	t.Helper()

	m := &clusterv1.Machine{}
	must(t, hub.Get(ctx, client.ObjectKey{Namespace: kvNamespace, Name: kvMachine}, m))

	return m.Annotations[quarantine.ReplaceStatusAnnotation]
}

func listRules(ctx context.Context, t *testing.T) []unstructured.Unstructured {
	t.Helper()

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ruleV1beta2.GroupVersion().WithKind(ruleV1beta2.Kind + "List"))
	must(t, hub.List(ctx, list))

	return list.Items
}

// countEventsIn counts the Events with the given reason about the named
// object of the given kind in a namespace.
func countEventsIn(ctx context.Context, t *testing.T, ns, kind, name, reason string) int {
	t.Helper()

	list := &eventsv1.EventList{}
	must(t, hub.List(ctx, list, client.InNamespace(ns)))
	n := 0
	for _, e := range list.Items {
		if e.Regarding.Kind == kind && e.Regarding.Name == name && e.Reason == reason {
			n++
		}
	}

	return n
}
