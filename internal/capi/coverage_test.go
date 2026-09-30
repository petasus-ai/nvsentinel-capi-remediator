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
	"errors"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var rebootTemplate = clusterv1.MachineHealthCheckRemediationTemplateReference{
	APIVersion: "infrastructure.example.com/v1alpha1",
	Kind:       "RebootRemediationTemplate",
	Name:       "reboot",
}

// newHealthCheck builds a MachineHealthCheck of cluster "gpu" that selects
// every one of its Machines, and applies mutations.
func newHealthCheck(name string, mutate ...func(*clusterv1.MachineHealthCheck)) clusterv1.MachineHealthCheck {
	mhc := clusterv1.MachineHealthCheck{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec:       clusterv1.MachineHealthCheckSpec{ClusterName: "gpu"},
	}
	for _, f := range mutate {
		f(&mhc)
	}

	return mhc
}

func withTemplate(mhc *clusterv1.MachineHealthCheck) {
	mhc.Spec.Remediation.TemplateRef = rebootTemplate
}

func pausedCheck(mhc *clusterv1.MachineHealthCheck) {
	mhc.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
}

func selecting(key, value string) func(*clusterv1.MachineHealthCheck) {
	return func(mhc *clusterv1.MachineHealthCheck) {
		mhc.Spec.Selector = metav1.LabelSelector{MatchLabels: map[string]string{key: value}}
	}
}

func names(c Coverage) []string {
	out := []string{}
	for _, check := range c.Checks {
		out = append(out, check.Name)
	}

	return out
}

func TestCoverageOfSelectsLikeClusterAPI(t *testing.T) {
	m := newMachine("w", func(m *clusterv1.Machine) { m.Labels["pool"] = "gpu-np" })

	tests := []struct {
		name  string
		check clusterv1.MachineHealthCheck
		want  bool
	}{
		{"empty selector covers the whole cluster", newHealthCheck("all"), true},
		{"matching selector", newHealthCheck("pool", selecting("pool", "gpu-np")), true},
		{"selector for another pool", newHealthCheck("other", selecting("pool", "cpu-np")), false},
		{"another cluster", newHealthCheck("elsewhere", func(mhc *clusterv1.MachineHealthCheck) {
			mhc.Spec.ClusterName = "cpu"
		}), false},
		{"another namespace", newHealthCheck("far", func(mhc *clusterv1.MachineHealthCheck) {
			mhc.Namespace = "other"
		}), false},
		{"selector that does not parse", newHealthCheck("broken", func(mhc *clusterv1.MachineHealthCheck) {
			mhc.Spec.Selector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "pool", Operator: "Bogus"},
			}}
		}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CoverageOf([]clusterv1.MachineHealthCheck{tt.check}, m).Covered(); got != tt.want {
				t.Fatalf("Covered() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCoverageOfSelectsOnlyTheMachinesCluster(t *testing.T) {
	// A check's selector gets the cluster-name label added, so a Machine
	// labelled for another Cluster is not selected even by an empty
	// selector in the right namespace.
	m := newMachine("w", func(m *clusterv1.Machine) { m.Labels[clusterv1.ClusterNameLabel] = "cpu" })
	if CoverageOf([]clusterv1.MachineHealthCheck{newHealthCheck("all")}, m).Covered() {
		t.Fatal("check covered a Machine labelled for another Cluster")
	}
}

func TestCoverageOfSortsAndKeepsTemplates(t *testing.T) {
	cov := CoverageOf([]clusterv1.MachineHealthCheck{
		newHealthCheck("b-reboot", withTemplate),
		newHealthCheck("a-replace"),
	}, newMachine("w"))

	if got, want := names(cov), []string{"a-replace", "b-reboot"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("checks = %q, want %q", got, want)
	}
	if cov.Checks[0].Template != nil {
		t.Fatal("check without a template reported one")
	}
	if cov.Checks[1].Template == nil || *cov.Checks[1].Template != rebootTemplate {
		t.Fatalf("template = %+v, want %+v", cov.Checks[1].Template, rebootTemplate)
	}
}

func TestCoveragePredicates(t *testing.T) {
	replace := newHealthCheck("replace")
	reboot := newHealthCheck("reboot", withTemplate)
	reboot2 := newHealthCheck("reboot-2", withTemplate)
	pausedReplace := newHealthCheck("paused-replace", pausedCheck)
	pausedReboot := newHealthCheck("paused-reboot", withTemplate, pausedCheck)

	tests := []struct {
		name          string
		checks        []clusterv1.MachineHealthCheck
		covered, only bool
		describe      string
	}{
		{"uncovered", nil, false, false,
			"no MachineHealthCheck selects the Machine"},
		{"replacement", []clusterv1.MachineHealthCheck{replace}, true, false,
			"MachineHealthCheck replace remediates by replacement"},
		{"template", []clusterv1.MachineHealthCheck{reboot}, true, true,
			"MachineHealthCheck reboot remediates through RebootRemediationTemplate reboot"},
		{"templates only", []clusterv1.MachineHealthCheck{reboot, reboot2}, true, true,
			"MachineHealthCheck reboot remediates through RebootRemediationTemplate reboot; " +
				"MachineHealthCheck reboot-2 remediates through RebootRemediationTemplate reboot"},
		{"mixed", []clusterv1.MachineHealthCheck{reboot, replace}, true, false,
			"MachineHealthCheck reboot remediates through RebootRemediationTemplate reboot; " +
				"MachineHealthCheck replace remediates by replacement"},
		// A paused check acts only at some time after it is unpaused, so it
		// covers nothing now.
		{"paused only", []clusterv1.MachineHealthCheck{pausedReplace}, false, false,
			"MachineHealthCheck paused-replace remediates by replacement (paused)"},
		{"paused template only", []clusterv1.MachineHealthCheck{pausedReboot}, false, false,
			"MachineHealthCheck paused-reboot remediates through RebootRemediationTemplate reboot (paused)"},
		{"template with a paused template", []clusterv1.MachineHealthCheck{reboot, pausedReboot}, true, true,
			"MachineHealthCheck paused-reboot remediates through RebootRemediationTemplate reboot (paused); " +
				"MachineHealthCheck reboot remediates through RebootRemediationTemplate reboot"},
		// ... but it still replaces the Machine once unpaused, which a
		// restart must not risk.
		{"template with a paused replacement", []clusterv1.MachineHealthCheck{reboot, pausedReplace}, true, false,
			"MachineHealthCheck paused-replace remediates by replacement (paused); " +
				"MachineHealthCheck reboot remediates through RebootRemediationTemplate reboot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cov := CoverageOf(tt.checks, newMachine("w"))
			if got := cov.Covered(); got != tt.covered {
				t.Errorf("Covered() = %v, want %v", got, tt.covered)
			}
			if got := cov.TemplatesOnly(); got != tt.only {
				t.Errorf("TemplatesOnly() = %v, want %v", got, tt.only)
			}
			if got := cov.Describe(); got != tt.describe {
				t.Errorf("Describe() = %q, want %q", got, tt.describe)
			}
		})
	}
}

func TestListHealthChecksKeepsTheClustersChecks(t *testing.T) {
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "gpu"}}
	objs := []client.Object{}
	for _, mhc := range []clusterv1.MachineHealthCheck{
		newHealthCheck("mine"),
		newHealthCheck("theirs", func(mhc *clusterv1.MachineHealthCheck) { mhc.Spec.ClusterName = "cpu" }),
		newHealthCheck("far", func(mhc *clusterv1.MachineHealthCheck) { mhc.Namespace = "other" }),
	} {
		objs = append(objs, mhc.DeepCopy())
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()

	checks, err := ListHealthChecks(context.Background(), c, cluster)
	if err != nil {
		t.Fatalf("ListHealthChecks: %v", err)
	}
	if len(checks) != 1 || checks[0].Name != "mine" {
		t.Fatalf("checks = %+v, want only mine", checks)
	}
}

func TestListHealthChecksReportsListFailure(t *testing.T) {
	boom := errors.New("boom")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return boom },
	}).Build()

	_, err := ListHealthChecks(context.Background(), c,
		&clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "gpu"}})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "default/gpu") {
		t.Fatalf("err = %v, want it to wrap %v and name the Cluster", err, boom)
	}
}

func TestCoverageIgnoringPause(t *testing.T) {
	cov := CoverageOf([]clusterv1.MachineHealthCheck{
		newHealthCheck("reboot", withTemplate, pausedCheck),
		newHealthCheck("replace", pausedCheck),
	}, newMachine("w"))
	if cov.Covered() {
		t.Fatal("paused checks count as coverage")
	}

	unpaused := cov.IgnoringPause()
	if !unpaused.Covered() || unpaused.TemplatesOnly() {
		t.Fatalf("unpaused coverage = %+v, want covered with a check remediating by replacement", unpaused)
	}
	for _, check := range unpaused.Checks {
		if check.Paused {
			t.Fatalf("check %s still paused", check.Name)
		}
	}
	// The original is left as it was.
	if !cov.Checks[0].Paused || !cov.Checks[1].Paused {
		t.Fatal("IgnoringPause changed the coverage it was called on")
	}
}
