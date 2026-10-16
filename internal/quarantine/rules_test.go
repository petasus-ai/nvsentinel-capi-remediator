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
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var ruleTime = time.Date(2026, 10, 15, 9, 30, 0, 0, time.UTC)

var owner = Owner{Namespace: ns, Name: "gpu-np-abc12-m", Cluster: cluster}

func newRules(t *testing.T, mapper meta.RESTMapper, objs ...client.Object) (*Rules, client.Client) {
	t.Helper()

	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithRESTMapper(mapper).WithObjects(objs...).Build()

	return &Rules{Client: c, Reader: c, Mapper: mapper, Now: func() time.Time { return ruleTime }}, c
}

func getRule(t *testing.T, c client.Client, gvk schema.GroupVersionKind, d Device) *unstructured.Unstructured {
	t.Helper()

	rule := &unstructured.Unstructured{}
	rule.SetGroupVersionKind(gvk)
	if err := c.Get(context.Background(), client.ObjectKey{Name: RuleName(d)}, rule); err != nil {
		t.Fatalf("get the rule of %s: %v", d, err)
	}

	return rule
}

func TestRulesEnsure(t *testing.T) {
	for _, c := range []struct {
		name   string
		served []schema.GroupVersionKind
		want   schema.GroupVersionKind
	}{
		{name: "Kubernetes 1.37 and later", served: []schema.GroupVersionKind{ruleV1GVK, ruleV1beta2GVK}, want: ruleV1GVK},
		{name: "Kubernetes 1.36 with the feature on", served: []schema.GroupVersionKind{ruleV1beta2GVK}, want: ruleV1beta2GVK},
	} {
		t.Run(c.name, func(t *testing.T) {
			rules, cl := newRules(t, newMapper(c.served...))
			ctx := context.Background()

			created, err := rules.Ensure(ctx, gpu0, owner)
			if err != nil || !created {
				t.Fatalf("Ensure = %v, %v; want the rule created", created, err)
			}

			rule := getRule(t, cl, c.want, gpu0)
			want := map[string]string{
				ManagedByLabel:        ManagedBy,
				MachineLabel:          owner.Name,
				MachineNamespaceLabel: ns,
				ClusterLabel:          cluster,
			}
			for k, v := range want {
				if got := rule.GetLabels()[k]; got != v {
					t.Errorf("label %s = %q; want %q", k, got, v)
				}
			}
			wantAnnotations := map[string]string{
				MachineAnnotation:     ns + "/" + owner.Name,
				DeviceAnnotation:      gpu0.String(),
				RequestedAtAnnotation: "2026-10-15T09:30:00Z",
			}
			for k, v := range wantAnnotations {
				if got := rule.GetAnnotations()[k]; got != v {
					t.Errorf("annotation %s = %q; want %q", k, got, v)
				}
			}
			selector, _, _ := unstructured.NestedStringMap(rule.Object, "spec", "deviceSelector")
			if selector["driver"] != gpu0.Driver || selector["pool"] != gpu0.Pool || selector["device"] != gpu0.Name || len(selector) != 3 {
				t.Errorf("deviceSelector = %v; want exactly %s", selector, gpu0)
			}
			taint, _, _ := unstructured.NestedStringMap(rule.Object, "spec", "taint")
			if taint["key"] != TaintKey || taint["value"] != "true" || taint["effect"] != "NoSchedule" || len(taint) != 3 {
				t.Errorf("taint = %v; want %s=true:NoSchedule", taint, TaintKey)
			}
			if len(rule.GetOwnerReferences()) > 0 {
				t.Errorf("the rule has owners %v; it must outlive the Machine", rule.GetOwnerReferences())
			}

			// Carrying out the request again changes nothing.
			created, err = rules.Ensure(ctx, gpu0, Owner{Namespace: ns, Name: "another", Cluster: cluster})
			if err != nil || created {
				t.Fatalf("second Ensure = %v, %v; want the rule found", created, err)
			}
			if got := getRule(t, cl, c.want, gpu0).GetLabels()[MachineLabel]; got != owner.Name {
				t.Errorf("the second Ensure changed the rule's Machine to %q", got)
			}
			if ok, err := rules.Exists(ctx, gpu0); err != nil || !ok {
				t.Errorf("Exists = %v, %v; want true", ok, err)
			}
			if ok, err := rules.Exists(ctx, gpu1); err != nil || ok {
				t.Errorf("Exists of a device without a rule = %v, %v; want false", ok, err)
			}
		})
	}
}

func TestRulesUnavailable(t *testing.T) {
	rules, _ := newRules(t, newMapper())
	if _, err := rules.Ensure(context.Background(), gpu0, owner); reasonOf(err) != ReasonDeviceTaintRuleUnavailable {
		t.Errorf("Ensure = %v; want a DeviceTaintRuleUnavailable rejection", err)
	}
	if _, err := rules.Exists(context.Background(), gpu0); reasonOf(err) != ReasonDeviceTaintRuleUnavailable {
		t.Errorf("Exists = %v; want a DeviceTaintRuleUnavailable rejection", err)
	}
}

func TestRulesConflict(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
		ok     bool
	}{
		{name: "another device", mutate: func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "gpu-9", "spec", "deviceSelector", "device")
		}},
		{name: "an effect that blocks nothing", mutate: func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "None", "spec", "taint", "effect")
		}},
		{name: "an effect that also evicts", mutate: func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "NoExecute", "spec", "taint", "effect")
		}, ok: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			existing := (&Rules{}).ruleFor(ruleV1GVK, gpu0, owner)
			c.mutate(existing)
			rules, _ := newRules(t, newMapper(ruleV1GVK), existing)

			_, err := rules.Ensure(context.Background(), gpu0, owner)
			if c.ok {
				if err != nil {
					t.Errorf("Ensure = %v; want the rule accepted", err)
				}
				return
			}
			if reasonOf(err) != ReasonDeviceTaintRuleConflict {
				t.Errorf("Ensure = %v; want a DeviceTaintRuleConflict rejection", err)
			}
		})
	}
}

func TestRuleLabelsOfLongNames(t *testing.T) {
	rules, cl := newRules(t, newMapper(ruleV1GVK))
	long := Owner{Namespace: ns, Name: strings.Repeat("m", 80), Cluster: strings.Repeat("c", 70)}
	if _, err := rules.Ensure(context.Background(), gpu0, long); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	rule := getRule(t, cl, ruleV1GVK, gpu0)
	for k, v := range rule.GetLabels() {
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			t.Errorf("label %s = %q is not a label value: %v", k, v, errs)
		}
	}
	if got := rule.GetAnnotations()[MachineAnnotation]; got != ns+"/"+long.Name {
		t.Errorf("annotation %s = %q; want the whole name", MachineAnnotation, got)
	}
}

func TestRulesOf(t *testing.T) {
	rules, cl := newRules(t, newMapper(ruleV1GVK))
	ctx := context.Background()
	for _, d := range []Device{gpu0, gpu1} {
		if _, err := rules.Ensure(ctx, d, owner); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rules.Ensure(ctx, nic0, Owner{Namespace: ns, Name: "another", Cluster: cluster}); err != nil {
		t.Fatal(err)
	}
	// A rule that no longer blocks anything does not count.
	weakened := getRule(t, cl, ruleV1GVK, gpu1)
	_ = unstructured.SetNestedField(weakened.Object, "None", "spec", "taint", "effect")
	if err := cl.Update(ctx, weakened); err != nil {
		t.Fatal(err)
	}

	got, err := rules.Of(ctx, owner)
	if err != nil || len(got) != 1 || got[0] != gpu0 {
		t.Errorf("Of = %v, %v; want [%s]", got, err, gpu0)
	}

	// Two long names shortened alike are told apart by the annotation.
	long := Owner{Namespace: ns, Name: strings.Repeat("m", 70) + "-a", Cluster: cluster}
	twin := Owner{Namespace: ns, Name: strings.Repeat("m", 70) + "-b", Cluster: cluster}
	other := Device{Driver: nicDriver, Pool: "node-a", Name: "nic-1"}
	if _, err := rules.Ensure(ctx, other, long); err != nil {
		t.Fatal(err)
	}
	if got, err := rules.Of(ctx, twin); err != nil || len(got) != 0 {
		t.Errorf("Of(twin) = %v, %v; want none", got, err)
	}
	if got, err := rules.Of(ctx, long); err != nil || len(got) != 1 || got[0] != other {
		t.Errorf("Of(long) = %v, %v; want [%s]", got, err, other)
	}
}

func TestCovers(t *testing.T) {
	for _, c := range []struct {
		devices []Device
		names   []string
		want    bool
	}{
		{devices: []Device{gpu0}, want: true},
		{devices: nil, want: false},
		{devices: []Device{gpu0, gpu1}, names: []string{"gpu-1"}, want: true},
		{devices: []Device{gpu0}, names: []string{"gpu-0", "gpu-1"}, want: false},
		{devices: []Device{gpu0}, names: []string{"gpu-1"}, want: false},
	} {
		if got := Covers(c.devices, c.names); got != c.want {
			t.Errorf("Covers(%v, %v) = %v; want %v", c.devices, c.names, got, c.want)
		}
	}
}
