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
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Locator finds the DRA devices allocated to the workload a Machine runs.
// A Rejection says why that cannot be done for the Machine.
type Locator interface {
	Devices(ctx context.Context, cluster *clusterv1.Cluster, m *clusterv1.Machine) ([]Device, error)
}

var (
	// kubevirtMachineKind, kubevirtClusterKind and vmiKind are read in
	// whichever version the cluster prefers. Only a few fields are read,
	// and they have kept their place across versions, so the providers are
	// not a dependency of this module.
	kubevirtMachineKind = schema.GroupKind{Group: clusterv1.GroupVersionInfrastructure.Group, Kind: "KubevirtMachine"}
	kubevirtClusterKind = schema.GroupKind{Group: clusterv1.GroupVersionInfrastructure.Group, Kind: "KubevirtCluster"}
	vmiKind             = schema.GroupKind{Group: "kubevirt.io", Kind: "VirtualMachineInstance"}
)

// createdByLabel is the label KubeVirt puts on a VMI's virt-launcher pods,
// with the VMI's UID as its value.
const createdByLabel = "kubevirt.io/created-by"

// vmiRunning is the phase of a VMI whose VM runs.
const vmiRunning = "Running"

// KubeVirtLocator finds the devices of a Cluster API Provider KubeVirt
// Machine: those allocated to the ResourceClaims of the virt-launcher pod
// that runs its VirtualMachineInstance. The VM has the name of the
// KubevirtMachine and lives in the namespace of its template, or else in
// the KubevirtMachine's own. They are the devices of the VM as it runs now:
// a restarted VM is given its devices anew, and may not hold the ones it
// held before.
type KubeVirtLocator struct {
	// Client reads the cluster the VMs run in. That is the management
	// cluster: a Machine whose VM runs in an external infrastructure
	// cluster is rejected. It should not be a cached client, which would
	// keep every pod of the cluster in memory for the few read here.
	Client client.Reader
	// Mapper finds the versions the cluster serves the KubeVirt kinds in.
	Mapper meta.RESTMapper
}

// Devices implements Locator.
func (l *KubeVirtLocator) Devices(ctx context.Context, cluster *clusterv1.Cluster, m *clusterv1.Machine) ([]Device, error) {
	ref := m.Spec.InfrastructureRef
	if ref.APIGroup != kubevirtMachineKind.Group || ref.Kind != kubevirtMachineKind.Kind {
		return nil, reject(ReasonNotKubeVirt, "the Machine's infrastructure is a %s of %s, not a %s",
			ref.Kind, ref.APIGroup, kubevirtMachineKind.Kind)
	}

	kvm, err := l.get(ctx, kubevirtMachineKind, client.ObjectKey{Namespace: m.Namespace, Name: ref.Name})
	if err != nil {
		return nil, err
	}
	if kvm == nil {
		return nil, reject(ReasonNoDevices, "the Machine's %s %s does not exist", kubevirtMachineKind.Kind, ref.Name)
	}
	if external, err := l.externalInfra(ctx, cluster, kvm); err != nil || external != "" {
		if err != nil {
			return nil, err
		}

		return nil, reject(ReasonExternalInfraCluster, "the Machine's VM runs in the cluster of the %s's infraClusterSecretRef", external)
	}

	vmNamespace, _, err := unstructured.NestedString(kvm.Object, "spec", "virtualMachineTemplate", "metadata", "namespace")
	if err != nil {
		return nil, fmt.Errorf("read the VM namespace of %s %s/%s: %w", kubevirtMachineKind.Kind, kvm.GetNamespace(), kvm.GetName(), err)
	}
	if vmNamespace == "" {
		vmNamespace = kvm.GetNamespace()
	}

	vmi, err := l.get(ctx, vmiKind, client.ObjectKey{Namespace: vmNamespace, Name: kvm.GetName()})
	if err != nil {
		return nil, err
	}
	// A VM that is starting, stopping or being restarted may be given other
	// devices than the ones it holds, and those it held may have been the
	// faulty ones. Only a running VM holds what the request is about.
	if vmi == nil {
		return nil, reject(ReasonNoDevices, "the Machine's VM %s/%s is not running", vmNamespace, kvm.GetName())
	}
	if vmi.GetDeletionTimestamp() != nil {
		return nil, reject(ReasonNoDevices, "the Machine's VM %s/%s is being deleted", vmNamespace, kvm.GetName())
	}
	if phase, _, _ := unstructured.NestedString(vmi.Object, "status", "phase"); phase != vmiRunning {
		return nil, reject(ReasonNoDevices, "the Machine's VM %s/%s is not running (phase %q)", vmNamespace, kvm.GetName(), phase)
	}

	claims, err := l.claimsOf(ctx, vmi)
	if err != nil {
		return nil, err
	}

	var devices []Device
	for _, name := range claims {
		claim := &resourcev1.ResourceClaim{}
		if err := l.Client.Get(ctx, client.ObjectKey{Namespace: vmNamespace, Name: name}, claim); err != nil {
			if apierrors.IsNotFound(err) {
				// A claim that is gone holds nothing any more.
				continue
			}

			return nil, fmt.Errorf("get ResourceClaim %s/%s: %w", vmNamespace, name, err)
		}
		if claim.Status.Allocation == nil {
			continue
		}
		for _, res := range claim.Status.Allocation.Devices.Results {
			// Admin access shares a device for monitoring and leaves it
			// to whoever else is allocated it.
			if res.AdminAccess != nil && *res.AdminAccess {
				continue
			}
			d := Device{Driver: res.Driver, Pool: res.Pool, Name: res.Device}
			if !slices.Contains(devices, d) {
				devices = append(devices, d)
			}
		}
	}
	slices.SortFunc(devices, compare)

	return devices, nil
}

// externalInfra names the object whose infraClusterSecretRef puts the VM in
// another cluster, the KubevirtMachine's or else the KubevirtCluster's, or
// returns "" when the VM runs in this one.
func (l *KubeVirtLocator) externalInfra(ctx context.Context, cluster *clusterv1.Cluster, kvm *unstructured.Unstructured) (string, error) {
	if hasSecretRef(kvm) {
		return kubevirtMachineKind.Kind, nil
	}

	ref := cluster.Spec.InfrastructureRef
	if ref.APIGroup != kubevirtClusterKind.Group || ref.Kind != kubevirtClusterKind.Kind {
		return "", nil
	}
	kvc, err := l.get(ctx, kubevirtClusterKind, client.ObjectKey{Namespace: cluster.Namespace, Name: ref.Name})
	if err != nil || kvc == nil {
		return "", err
	}
	if hasSecretRef(kvc) {
		return kubevirtClusterKind.Kind, nil
	}

	return "", nil
}

// hasSecretRef reports whether a KubevirtMachine or KubevirtCluster names a
// kubeconfig for an external infrastructure cluster.
func hasSecretRef(o *unstructured.Unstructured) bool {
	ref, found, _ := unstructured.NestedMap(o.Object, "spec", "infraClusterSecretRef")
	return found && len(ref) > 0
}

// claimsOf returns the ResourceClaims of the VMI's active virt-launcher
// pods: those the VMI lists as active, or, when it lists none, every one
// that has not terminated.
func (l *KubeVirtLocator) claimsOf(ctx context.Context, vmi *unstructured.Unstructured) ([]string, error) {
	active, _, _ := unstructured.NestedStringMap(vmi.Object, "status", "activePods")

	pods := &corev1.PodList{}
	if err := l.Client.List(ctx, pods,
		client.InNamespace(vmi.GetNamespace()),
		client.MatchingLabels{createdByLabel: string(vmi.GetUID())},
	); err != nil {
		return nil, fmt.Errorf("list the virt-launcher pods of %s/%s: %w", vmi.GetNamespace(), vmi.GetName(), err)
	}

	var claims []string
	add := func(name *string) {
		if name != nil && *name != "" && !slices.Contains(claims, *name) {
			claims = append(claims, *name)
		}
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if _, ok := active[string(p.UID)]; len(active) > 0 && !ok {
			continue
		}
		// A claim the pod names itself, and one created for it from a
		// template, which only its status names.
		for _, c := range p.Spec.ResourceClaims {
			add(c.ResourceClaimName)
		}
		for _, s := range p.Status.ResourceClaimStatuses {
			add(s.ResourceClaimName)
		}
	}

	return claims, nil
}

// get reads an object of a kind in the version the cluster prefers. It
// returns nil when the object does not exist, and a Rejection when the
// cluster does not serve the kind at all.
func (l *KubeVirtLocator) get(ctx context.Context, gk schema.GroupKind, key client.ObjectKey) (*unstructured.Unstructured, error) {
	mapping, err := l.Mapper.RESTMapping(gk)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return nil, reject(ReasonNotKubeVirt, "the cluster does not serve %s.%s", gk.Kind, gk.Group)
		}

		return nil, fmt.Errorf("find the version of %s.%s: %w", gk.Kind, gk.Group, err)
	}

	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(mapping.GroupVersionKind)
	if err := l.Client.Get(ctx, key, o); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("get %s %s: %w", gk.Kind, key, err)
	}

	return o, nil
}
