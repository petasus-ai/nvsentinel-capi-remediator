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
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// deviceTaintRuleKind is the kind of the rules.
var deviceTaintRuleKind = schema.GroupKind{Group: "resource.k8s.io", Kind: "DeviceTaintRule"}

// ruleVersions are the versions of DeviceTaintRule this operator writes,
// most preferred first. The rules are generally available, and served by
// default, from Kubernetes 1.37 in v1; 1.36 serves them in v1beta2 only
// once the DRADeviceTaintRules feature gate and the API are enabled. The
// fields written are the same in both, as of Kubernetes 1.37.
var ruleVersions = []string{"v1", "v1beta2"}

// blockingEffects are the taint effects that keep a device from being
// allocated: NoSchedule, which the rules here carry, and NoExecute, which
// also evicts whoever uses the device.
var blockingEffects = []string{"NoSchedule", "NoExecute"}

// Owner is the Machine a rule is created for.
type Owner struct {
	Namespace string
	Name      string
	Cluster   string
}

// Rules creates and checks the DeviceTaintRules that quarantine devices,
// through whichever version of the API the cluster serves. Rules are
// cluster-scoped and have no owner: a quarantine has to outlive the
// Machine it was asked for, until an operator deletes the rule.
type Rules struct {
	// Client writes to the cluster the devices are in.
	Client client.Writer
	// Reader reads from it. It should not be a cached reader: the rules are
	// read right after they are written, to make sure they are there.
	Reader client.Reader
	// Mapper finds the version of DeviceTaintRule the cluster serves.
	Mapper meta.RESTMapper
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Ensure makes sure the device is quarantined: it creates the device's rule
// unless that rule is already there. It returns whether it created it.
func (r *Rules) Ensure(ctx context.Context, d Device, owner Owner) (bool, error) {
	gvk, err := r.kind()
	if err != nil {
		return false, err
	}

	rule := r.ruleFor(gvk, d, owner)
	if err := r.Client.Create(ctx, rule); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return false, r.check(ctx, gvk, d)
		}

		return false, fmt.Errorf("create DeviceTaintRule %s for %s: %w", rule.GetName(), d, err)
	}

	return true, nil
}

// Of returns the devices whose rules were created for the Machine and
// keep them from being allocated, found by the rules' labels. A device
// quarantined for another Machine first keeps that Machine's labels, and is
// not among them.
func (r *Rules) Of(ctx context.Context, owner Owner) ([]Device, error) {
	gvk, err := r.kind()
	if err != nil {
		return nil, err
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := r.Reader.List(ctx, list, client.MatchingLabels{
		ManagedByLabel:        ManagedBy,
		MachineLabel:          labelValue(owner.Name),
		MachineNamespaceLabel: labelValue(owner.Namespace),
	}); err != nil {
		return nil, fmt.Errorf("list the DeviceTaintRules of %s/%s: %w", owner.Namespace, owner.Name, err)
	}

	var devices []Device
	for i := range list.Items {
		rule := &list.Items[i]
		// A shortened label may stand for another Machine; the annotation
		// has the whole name.
		if rule.GetAnnotations()[MachineAnnotation] != owner.Namespace+"/"+owner.Name {
			continue
		}
		selector, _, _ := unstructured.NestedStringMap(rule.Object, "spec", "deviceSelector")
		effect, _, _ := unstructured.NestedString(rule.Object, "spec", "taint", "effect")
		d := Device{Driver: selector["driver"], Pool: selector["pool"], Name: selector["device"]}
		if d.Driver == "" || d.Pool == "" || d.Name == "" || !slices.Contains(blockingEffects, effect) ||
			rule.GetName() != RuleName(d) {
			continue
		}
		devices = append(devices, d)
	}
	slices.SortFunc(devices, compare)

	return devices, nil
}

// Covers reports whether the devices are what a request names: every name
// it lists is among them, or, for a request that lists none, there is at
// least one.
func Covers(devices []Device, names []string) bool {
	if len(devices) == 0 {
		return false
	}
	for _, name := range names {
		if !slices.ContainsFunc(devices, func(d Device) bool { return d.Name == name }) {
			return false
		}
	}

	return true
}

// Exists reports whether the device's rule is in place.
func (r *Rules) Exists(ctx context.Context, d Device) (bool, error) {
	gvk, err := r.kind()
	if err != nil {
		return false, err
	}

	if err := r.check(ctx, gvk, d); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

// check reads the device's rule and makes sure that it selects the device
// and keeps it from being allocated. A rule of that name that does not is
// someone else's, and is left alone with a Rejection.
func (r *Rules) check(ctx context.Context, gvk schema.GroupVersionKind, d Device) error {
	rule := &unstructured.Unstructured{}
	rule.SetGroupVersionKind(gvk)
	name := RuleName(d)
	if err := r.Reader.Get(ctx, client.ObjectKey{Name: name}, rule); err != nil {
		if apierrors.IsNotFound(err) {
			return err
		}

		return fmt.Errorf("get DeviceTaintRule %s for %s: %w", name, d, err)
	}

	selector, _, _ := unstructured.NestedStringMap(rule.Object, "spec", "deviceSelector")
	effect, _, _ := unstructured.NestedString(rule.Object, "spec", "taint", "effect")
	if selector["driver"] != d.Driver || selector["pool"] != d.Pool || selector["device"] != d.Name ||
		!slices.Contains(blockingEffects, effect) {
		return reject(ReasonDeviceTaintRuleConflict,
			"DeviceTaintRule %s exists but does not taint %s with one of %s", name, d, strings.Join(blockingEffects, ", "))
	}

	return nil
}

// kind returns the kind of DeviceTaintRule in the version to use.
//
// The versions are looked up one at a time. controller-runtime's mapper,
// asked for several at once, reloads the group for all of them and can fail
// when one of them is not served, which would hide a version that is.
func (r *Rules) kind() (schema.GroupVersionKind, error) {
	for _, version := range ruleVersions {
		mapping, err := r.Mapper.RESTMapping(deviceTaintRuleKind, version)
		if err == nil {
			return mapping.GroupVersionKind, nil
		}
		if !meta.IsNoMatchError(err) {
			return schema.GroupVersionKind{}, fmt.Errorf("find %s.%s in %s: %w", deviceTaintRuleKind.Kind, deviceTaintRuleKind.Group, version, err)
		}
	}

	return schema.GroupVersionKind{}, reject(ReasonDeviceTaintRuleUnavailable,
		"the cluster serves %s.%s in none of the versions %s; Kubernetes 1.36 needs the DRADeviceTaintRules feature gate and the %s/v1beta2 API enabled",
		deviceTaintRuleKind.Kind, deviceTaintRuleKind.Group, strings.Join(ruleVersions, ", "), deviceTaintRuleKind.Group)
}

// ruleFor builds the rule that quarantines the device.
func (r *Rules) ruleFor(gvk schema.GroupVersionKind, d Device, owner Owner) *unstructured.Unstructured {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}

	rule := &unstructured.Unstructured{}
	rule.SetGroupVersionKind(gvk)
	rule.SetName(RuleName(d))
	rule.SetLabels(map[string]string{
		ManagedByLabel:        ManagedBy,
		MachineLabel:          labelValue(owner.Name),
		MachineNamespaceLabel: labelValue(owner.Namespace),
		ClusterLabel:          labelValue(owner.Cluster),
	})
	rule.SetAnnotations(map[string]string{
		MachineAnnotation:     owner.Namespace + "/" + owner.Name,
		DeviceAnnotation:      d.String(),
		RequestedAtAnnotation: now().UTC().Format(time.RFC3339),
	})
	rule.Object["spec"] = map[string]any{
		"deviceSelector": map[string]any{
			"driver": d.Driver,
			"pool":   d.Pool,
			"device": d.Name,
		},
		"taint": map[string]any{
			"key":    TaintKey,
			"value":  "true",
			"effect": "NoSchedule",
		},
	}

	return rule
}
