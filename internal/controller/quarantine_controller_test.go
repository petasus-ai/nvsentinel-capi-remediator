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

package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/capi"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/quarantine"
)

const (
	quarantineMachine = "gpu-np-abc12"
	gpuDriver         = "gpu.example.com"
	// keepFinalizer holds a deleted Machine back, the way Cluster API's
	// own finalizer does while it drains the node, so that a test can
	// look at it after the deletion.
	keepFinalizer = "test.example.com/keep"
)

var (
	ruleGVK  = schema.GroupVersionKind{Group: "resource.k8s.io", Version: "v1beta2", Kind: "DeviceTaintRule"}
	quarGPU0 = quarantine.Device{Driver: gpuDriver, Pool: "node-a", Name: "gpu-0"}
	quarGPU1 = quarantine.Device{Driver: gpuDriver, Pool: "node-a", Name: "gpu-1"}
	quarNIC0 = quarantine.Device{Driver: "nic.example.com", Pool: "node-a", Name: "nic-0"}
)

// stubLocator hands out a fixed answer and counts how often it is asked.
type stubLocator struct {
	devices []quarantine.Device
	err     error
	calls   int
}

func (s *stubLocator) Devices(context.Context, *clusterv1.Cluster, *clusterv1.Machine) ([]quarantine.Device, error) {
	s.calls++
	return s.devices, s.err
}

// quarantineMapper serves the Cluster API kinds and, when given, a version
// of DeviceTaintRule.
func quarantineMapper(rules ...schema.GroupVersionKind) meta.RESTMapper {
	gvs := []schema.GroupVersion{clusterv1.GroupVersion}
	for _, gvk := range rules {
		gvs = append(gvs, gvk.GroupVersion())
	}
	m := meta.NewDefaultRESTMapper(gvs)
	m.Add(clusterv1.GroupVersion.WithKind("Machine"), meta.RESTScopeNamespace)
	m.Add(clusterv1.GroupVersion.WithKind("Cluster"), meta.RESTScopeNamespace)
	for _, gvk := range rules {
		m.Add(gvk, meta.RESTScopeRoot)
	}

	return m
}

// replaceable is a worker Machine a MachineSet owns, with a replace request.
func replaceable(request string, mutate ...func(*clusterv1.Machine)) *clusterv1.Machine {
	m := newMachine(quarantineMachine, quarantineMachine, func(m *clusterv1.Machine) {
		m.Annotations = map[string]string{quarantine.ReplaceAnnotation: request}
		m.Finalizers = []string{keepFinalizer}
		m.UID = "machine-uid"
		m.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: clusterv1.GroupVersion.String(), Kind: "MachineSet", Name: "gpu-np", UID: "ms-uid",
			Controller: ptrTo(true),
		}}
	})
	for _, f := range mutate {
		f(m)
	}

	return m
}

func ptrTo[T any](v T) *T { return &v }

type quarantineFixture struct {
	hub      client.Client
	locator  *stubLocator
	recorder *events.FakeRecorder
	r        *MachineQuarantineReconciler
}

type quarantineOptions struct {
	dryRun   bool
	devices  []quarantine.Device
	err      error
	mapper   meta.RESTMapper
	funcs    interceptor.Funcs
	selector labels.Selector
}

func newQuarantineFixture(t *testing.T, opts quarantineOptions, objs ...client.Object) *quarantineFixture {
	t.Helper()

	mapper := opts.mapper
	if mapper == nil {
		mapper = quarantineMapper(ruleGVK)
	}
	hub := fake.NewClientBuilder().WithScheme(newScheme(t)).WithRESTMapper(mapper).
		WithObjects(objs...).WithInterceptorFuncs(opts.funcs).Build()
	locator := &stubLocator{devices: opts.devices, err: opts.err}
	recorder := events.NewFakeRecorder(16)

	return &quarantineFixture{
		hub:      hub,
		locator:  locator,
		recorder: recorder,
		r: &MachineQuarantineReconciler{
			Client:          hub,
			Locator:         locator,
			Rules:           &quarantine.Rules{Client: hub, Reader: hub, Mapper: mapper, Now: func() time.Time { return fixtureTime }},
			Drivers:         sets.New(gpuDriver),
			Recorder:        recorder,
			DryRun:          opts.dryRun,
			ClusterSelector: opts.selector,
		},
	}
}

func (f *quarantineFixture) reconcile(t *testing.T) error {
	t.Helper()

	_, err := f.reconcileResult(t)

	return err
}

func (f *quarantineFixture) reconcileResult(t *testing.T) (ctrl.Result, error) {
	t.Helper()

	return f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: quarantineMachine}})
}

func (f *quarantineFixture) machine(t *testing.T) *clusterv1.Machine {
	t.Helper()

	m := &clusterv1.Machine{}
	if err := f.hub.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: quarantineMachine}, m); err != nil {
		t.Fatalf("get machine: %v", err)
	}

	return m
}

func (f *quarantineFixture) rules(t *testing.T) []unstructured.Unstructured {
	t.Helper()

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ruleGVK.GroupVersion().WithKind(ruleGVK.Kind + "List"))
	if err := f.hub.List(context.Background(), list); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		t.Fatalf("list rules: %v", err)
	}

	return list.Items
}

func (f *quarantineFixture) events() []string {
	var out []string
	for {
		select {
		case e := <-f.recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestQuarantineReplacesTheMachine(t *testing.T) {
	f := newQuarantineFixture(t, quarantineOptions{devices: []quarantine.Device{quarNIC0, quarGPU0}},
		newCluster(), replaceable(quarantine.RequestQuarantine))

	if err := f.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	m := f.machine(t)
	if m.DeletionTimestamp.IsZero() {
		t.Error("the Machine was not deleted")
	}
	if got := m.Annotations[quarantine.ReplaceStatusAnnotation]; got != quarantine.StatusReplacing {
		t.Errorf("status = %q; want %q", got, quarantine.StatusReplacing)
	}
	rules := f.rules(t)
	if len(rules) != 1 || rules[0].GetName() != quarantine.RuleName(quarGPU0) {
		t.Fatalf("rules = %v; want the one of %s", rules, quarGPU0)
	}
	if got := rules[0].GetLabels()[quarantine.MachineLabel]; got != quarantineMachine {
		t.Errorf("the rule's Machine label = %q; want %q", got, quarantineMachine)
	}
	assertEvents(t, f.events(),
		"Warning "+EventDevicesQuarantined+" Quarantined with DeviceTaintRules: "+quarGPU0.String()+" ("+quarantine.RuleName(quarGPU0)+")",
		"Warning "+EventMachineReplaced+" Deleted for its MachineSet to replace")

	// The Machine's deletion and the status change wake the controller
	// again; neither does anything more.
	if err := f.reconcile(t); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if f.locator.calls != 1 {
		t.Errorf("the devices were looked up %d times; want once", f.locator.calls)
	}
	assertEvents(t, f.events())
}

func TestQuarantineListedDevices(t *testing.T) {
	f := newQuarantineFixture(t, quarantineOptions{devices: []quarantine.Device{quarGPU0, quarGPU1}},
		newCluster(), replaceable("quarantine:gpu-1,gpu-0"))

	if err := f.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := len(f.rules(t)); got != 2 {
		t.Errorf("%d rules; want one for each listed device", got)
	}
	if f.machine(t).DeletionTimestamp.IsZero() {
		t.Error("the Machine was not deleted")
	}
}

func TestQuarantineResumesAnInterruptedRequest(t *testing.T) {
	// The rule was made before the operator stopped; the Machine was not
	// deleted yet.
	rule := ruleOf(quarGPU0, quarantineMachine)
	for _, status := range []string{quarantine.StatusQuarantining, quarantine.StatusReplacing} {
		t.Run(status, func(t *testing.T) {
			f := newQuarantineFixture(t, quarantineOptions{devices: []quarantine.Device{quarGPU0}},
				newCluster(), rule.DeepCopy(), replaceable(quarantine.RequestQuarantine, func(m *clusterv1.Machine) {
					m.Annotations[quarantine.ReplaceStatusAnnotation] = status
				}))

			if err := f.reconcile(t); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if f.machine(t).DeletionTimestamp.IsZero() {
				t.Error("the Machine was not deleted")
			}
			if got := len(f.rules(t)); got != 1 {
				t.Errorf("%d rules; want the one there was", got)
			}
			want := []string{EventDevicesQuarantined, EventMachineReplaced}
			lookups := 1
			if status == quarantine.StatusReplacing {
				// Every rule was in place before the interruption, and the
				// Event recorded. The devices are not looked up again: the
				// VM may hold others by now.
				want = want[1:]
				lookups = 0
			}
			assertEvents(t, f.events(), want...)
			if f.locator.calls != lookups {
				t.Errorf("the devices were looked up %d times; want %d", f.locator.calls, lookups)
			}
		})
	}
}

// ruleOf is the rule the operator makes for a device on behalf of the named
// Machine.
func ruleOf(d quarantine.Device, machine string) *unstructured.Unstructured {
	rule := &unstructured.Unstructured{}
	rule.SetGroupVersionKind(ruleGVK)
	rule.SetName(quarantine.RuleName(d))
	rule.SetLabels(map[string]string{
		quarantine.ManagedByLabel:        quarantine.ManagedBy,
		quarantine.MachineLabel:          machine,
		quarantine.MachineNamespaceLabel: testNamespace,
		quarantine.ClusterLabel:          testCluster,
	})
	rule.SetAnnotations(map[string]string{quarantine.MachineAnnotation: testNamespace + "/" + machine})
	rule.Object["spec"] = map[string]any{
		"deviceSelector": map[string]any{"driver": d.Driver, "pool": d.Pool, "device": d.Name},
		"taint":          map[string]any{"key": quarantine.TaintKey, "value": "true", "effect": "NoSchedule"},
	}

	return rule
}

func TestQuarantineResumesOnlyARequestItsRulesCover(t *testing.T) {
	replacing := func(request string) *clusterv1.Machine {
		return replaceable(request, func(m *clusterv1.Machine) {
			m.Annotations[quarantine.ReplaceStatusAnnotation] = quarantine.StatusReplacing
		})
	}
	for _, c := range []struct {
		name    string
		request string
		rules   []client.Object
		// want are the devices with rules once the Machine is deleted.
		want []string
	}{
		{name: "no rules, as with a status set by hand", request: "quarantine:gpu-0",
			want: []string{quarantine.RuleName(quarGPU0)}},
		{name: "the request edited since", request: "quarantine:gpu-1", rules: []client.Object{ruleOf(quarGPU0, quarantineMachine)},
			want: []string{quarantine.RuleName(quarGPU0), quarantine.RuleName(quarGPU1)}},
		{name: "rules made for another Machine", request: "quarantine:gpu-0", rules: []client.Object{ruleOf(quarGPU0, "other")},
			want: []string{quarantine.RuleName(quarGPU0)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			objs := append([]client.Object{newCluster(), replacing(c.request)}, c.rules...)
			f := newQuarantineFixture(t, quarantineOptions{devices: []quarantine.Device{quarGPU0, quarGPU1}}, objs...)

			if err := f.reconcile(t); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if f.locator.calls != 1 {
				t.Errorf("the devices were looked up %d times; want the request carried out again", f.locator.calls)
			}
			if f.machine(t).DeletionTimestamp.IsZero() {
				t.Error("the Machine was not deleted")
			}
			var got []string
			for _, r := range f.rules(t) {
				got = append(got, r.GetName())
			}
			slices.Sort(got)
			slices.Sort(c.want)
			if !slices.Equal(got, c.want) {
				t.Errorf("rules = %v; want %v", got, c.want)
			}
			assertEvents(t, f.events(), EventDevicesQuarantined, EventMachineReplaced)
		})
	}
}

func TestQuarantineDryRunWritesNothing(t *testing.T) {
	f := newQuarantineFixture(t, quarantineOptions{dryRun: true, devices: []quarantine.Device{quarGPU0}},
		newCluster(), replaceable(quarantine.RequestQuarantine))

	if err := f.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	m := f.machine(t)
	if !m.DeletionTimestamp.IsZero() || m.Annotations[quarantine.ReplaceStatusAnnotation] != "" {
		t.Errorf("a dry run changed the Machine: deleted=%v annotations=%v", !m.DeletionTimestamp.IsZero(), m.Annotations)
	}
	if rules := f.rules(t); len(rules) > 0 {
		t.Errorf("a dry run made rules %v", rules)
	}
	if f.locator.calls != 1 {
		t.Errorf("a dry run looked the devices up %d times; want once", f.locator.calls)
	}
	assertEvents(t, f.events())

	// A rejection is only logged, too.
	f = newQuarantineFixture(t, quarantineOptions{dryRun: true},
		newCluster(), replaceable(quarantine.RequestQuarantine, controlPlane))
	if err := f.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := f.machine(t).Annotations[quarantine.ReplaceStatusAnnotation]; got != "" {
		t.Errorf("a dry run set the status %q", got)
	}
	assertEvents(t, f.events())
}

func TestQuarantineRejects(t *testing.T) {
	forbidden := interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		return apierrors.NewForbidden(schema.GroupResource{Group: "resource.k8s.io", Resource: "devicetaintrules"}, o.GetName(), errors.New("no"))
	}}

	cases := []struct {
		name    string
		request string
		machine func(*clusterv1.Machine)
		cluster func(*clusterv1.Cluster)
		opts    quarantineOptions
		reason  quarantine.Reason
		// looked is whether the devices are looked up before the request
		// is rejected.
		looked bool
		// keepsRules is whether the request is rejected once its rules
		// are made.
		keepsRules bool
	}{
		{name: "a control plane Machine", machine: controlPlane, reason: quarantine.Reason(capi.SkipControlPlane)},
		{name: "a paused Machine", machine: func(m *clusterv1.Machine) { m.Annotations[clusterv1.PausedAnnotation] = "" },
			reason: quarantine.Reason(capi.SkipPaused)},
		{name: "a Machine opted out of remediation", machine: func(m *clusterv1.Machine) {
			m.Annotations[clusterv1.MachineSkipRemediationAnnotation] = ""
		}, reason: quarantine.Reason(capi.SkipOptedOut)},
		{name: "a paused Cluster", cluster: func(c *clusterv1.Cluster) { c.Spec.Paused = ptrTo(true) }, reason: QuarantineClusterPaused},
		{name: "a Machine marked for remediation", machine: func(m *clusterv1.Machine) {
			m.Annotations[clusterv1.RemediateMachineAnnotation] = ""
			m.Annotations[capi.RemediationActionAnnotation] = capi.ActionRestart
		}, reason: quarantine.Reason(capi.SkipAlreadyMarked)},
		{name: "a Machine no MachineSet owns", machine: func(m *clusterv1.Machine) { m.OwnerReferences = nil },
			reason: QuarantineNotReplaceable},
		{name: "a Machine some other kind owns", machine: func(m *clusterv1.Machine) { m.OwnerReferences[0].Kind = "MachinePool" },
			reason: QuarantineNotReplaceable},
		{name: "an invalid request", request: "quarantine-gpu", reason: quarantine.ReasonInvalidRequest},
		{name: "the locator's rejection", opts: quarantineOptions{err: &quarantine.Rejection{Reason: quarantine.ReasonNotKubeVirt, Message: "no"}},
			reason: quarantine.ReasonNotKubeVirt, looked: true},
		{name: "a lookup that is not allowed", opts: quarantineOptions{err: apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no"))},
			reason: quarantine.ReasonForbidden, looked: true},
		{name: "several devices without a list", opts: quarantineOptions{devices: []quarantine.Device{quarGPU0, quarGPU1}},
			reason: quarantine.ReasonAmbiguousDevices, looked: true},
		{name: "an unknown device", request: "quarantine:gpu-7", opts: quarantineOptions{devices: []quarantine.Device{quarGPU0}},
			reason: quarantine.ReasonUnknownDevice, looked: true},
		{name: "no DeviceTaintRule served", opts: quarantineOptions{devices: []quarantine.Device{quarGPU0}, mapper: quarantineMapper()},
			reason: quarantine.ReasonDeviceTaintRuleUnavailable, looked: true},
		{name: "rules not allowed", opts: quarantineOptions{devices: []quarantine.Device{quarGPU0}, funcs: forbidden},
			reason: quarantine.ReasonForbidden, looked: true},
		{name: "the deletion not allowed", opts: quarantineOptions{devices: []quarantine.Device{quarGPU0}, funcs: interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
				return apierrors.NewForbidden(schema.GroupResource{Group: clusterv1.GroupVersion.Group, Resource: "machines"}, o.GetName(), errors.New("no"))
			},
		}}, reason: quarantine.ReasonForbidden, looked: true, keepsRules: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			request := c.request
			if request == "" {
				request = quarantine.RequestQuarantine
			}
			var machineMutations []func(*clusterv1.Machine)
			if c.machine != nil {
				machineMutations = append(machineMutations, c.machine)
			}
			var clusterMutations []func(*clusterv1.Cluster)
			if c.cluster != nil {
				clusterMutations = append(clusterMutations, c.cluster)
			}
			f := newQuarantineFixture(t, c.opts, newCluster(clusterMutations...), replaceable(request, machineMutations...))

			if err := f.reconcile(t); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			m := f.machine(t)
			if got, want := m.Annotations[quarantine.ReplaceStatusAnnotation], quarantine.RejectedStatus(c.reason); got != want {
				t.Errorf("status = %q; want %q", got, want)
			}
			if !m.DeletionTimestamp.IsZero() {
				t.Error("a rejected request deleted the Machine")
			}
			if c.opts.mapper == nil {
				if rules := f.rules(t); len(rules) > 0 != c.keepsRules {
					t.Errorf("rules after the rejection = %v; want some: %v", rules, c.keepsRules)
				}
			}
			if looked := f.locator.calls > 0; looked != c.looked {
				t.Errorf("devices looked up: %v; want %v", looked, c.looked)
			}
			want := []string{"Warning " + EventQuarantineRejected + ` Replace request "` + request + `" rejected (` + string(c.reason) + ")"}
			if c.keepsRules {
				want = append([]string{EventDevicesQuarantined}, want...)
			}
			assertEvents(t, f.events(), want...)

			// A rejected request stays rejected.
			if err := f.reconcile(t); err != nil {
				t.Fatalf("second Reconcile: %v", err)
			}
			assertEvents(t, f.events())
		})
	}
}

func TestQuarantineTriesAgainOnceTheStatusIsRemoved(t *testing.T) {
	f := newQuarantineFixture(t, quarantineOptions{devices: []quarantine.Device{quarGPU0, quarGPU1}},
		newCluster(), replaceable(quarantine.RequestQuarantine))
	if err := f.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.events()

	// The operator lists the device and removes the status.
	m := f.machine(t)
	m.Annotations[quarantine.ReplaceAnnotation] = "quarantine:gpu-1"
	delete(m.Annotations, quarantine.ReplaceStatusAnnotation)
	if err := f.hub.Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if f.machine(t).DeletionTimestamp.IsZero() {
		t.Error("the Machine was not deleted")
	}
	if rules := f.rules(t); len(rules) != 1 || rules[0].GetName() != quarantine.RuleName(quarGPU1) {
		t.Errorf("rules = %v; want the one of %s", rules, quarGPU1)
	}
}

func TestQuarantineWithdrawnRequestDropsItsStatus(t *testing.T) {
	m := replaceable("", func(m *clusterv1.Machine) {
		delete(m.Annotations, quarantine.ReplaceAnnotation)
		m.Annotations[quarantine.ReplaceStatusAnnotation] = quarantine.RejectedStatus(quarantine.ReasonNoDevices)
	})
	f := newQuarantineFixture(t, quarantineOptions{}, newCluster(), m)

	if err := f.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := f.machine(t).Annotations[quarantine.ReplaceStatusAnnotation]; ok {
		t.Error("the status of a withdrawn request stayed")
	}
	if f.locator.calls > 0 {
		t.Error("a withdrawn request looked the devices up")
	}
}

func TestQuarantineLeavesOtherClustersAlone(t *testing.T) {
	selector := labels.SelectorFromSet(labels.Set{"environment": "gpu"})
	for name, objs := range map[string][]client.Object{
		"a Cluster outside the selector": {newCluster(), replaceable(quarantine.RequestQuarantine)},
		"a Cluster that is gone":         {replaceable(quarantine.RequestQuarantine)},
	} {
		t.Run(name, func(t *testing.T) {
			f := newQuarantineFixture(t, quarantineOptions{devices: []quarantine.Device{quarGPU0}, selector: selector}, objs...)
			if err := f.reconcile(t); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			m := f.machine(t)
			if !m.DeletionTimestamp.IsZero() || m.Annotations[quarantine.ReplaceStatusAnnotation] != "" {
				t.Errorf("the Machine was acted on: deleted=%v annotations=%v", !m.DeletionTimestamp.IsZero(), m.Annotations)
			}
			assertEvents(t, f.events())
		})
	}
}

func TestQuarantineRetriesTransientFailures(t *testing.T) {
	f := newQuarantineFixture(t, quarantineOptions{err: errors.New("connection refused")},
		newCluster(), replaceable(quarantine.RequestQuarantine))

	if err := f.reconcile(t); err == nil {
		t.Fatal("Reconcile succeeded; want the failure returned for a retry")
	}
	if got := f.machine(t).Annotations[quarantine.ReplaceStatusAnnotation]; got != "" {
		t.Errorf("status = %q; want none", got)
	}
	assertEvents(t, f.events())
}

func TestReplaceRequestChanged(t *testing.T) {
	p := replaceRequestChanged()
	plain := newMachine("m", "n", func(m *clusterv1.Machine) { m.Annotations = map[string]string{"other": "x"} })
	requested := plain.DeepCopy()
	requested.Annotations[quarantine.ReplaceAnnotation] = quarantine.RequestQuarantine
	statused := requested.DeepCopy()
	statused.Annotations[quarantine.ReplaceStatusAnnotation] = quarantine.StatusQuarantining
	relisted := requested.DeepCopy()
	relisted.Annotations[quarantine.ReplaceAnnotation] = "quarantine:gpu-0"
	touched := requested.DeepCopy()
	touched.Labels["touched"] = "yes"
	touched.Status.Phase = "Running"

	if p.Create(event.CreateEvent{Object: plain}) {
		t.Error("a Machine without a request was admitted")
	}
	if !p.Create(event.CreateEvent{Object: requested}) {
		t.Error("a Machine with a request was not admitted")
	}
	for name, c := range map[string]struct {
		old, new *clusterv1.Machine
		want     bool
	}{
		"request added":     {plain, requested, true},
		"request removed":   {requested, plain, true},
		"request changed":   {requested, relisted, true},
		"status changed":    {requested, statused, true},
		"status removed":    {statused, requested, true},
		"something else":    {requested, touched, false},
		"no request at all": {plain, plain, false},
	} {
		if got := p.Update(event.UpdateEvent{ObjectOld: c.old, ObjectNew: c.new}); got != c.want {
			t.Errorf("%s: admitted = %v; want %v", name, got, c.want)
		}
	}
	if p.Delete(event.DeleteEvent{Object: requested}) {
		t.Error("a deleted Machine was admitted")
	}
}

func TestQuarantineDeletesNothingWithoutItsRules(t *testing.T) {
	// The rule is created, but reading it back finds nothing.
	hidden := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
		if u, ok := o.(*unstructured.Unstructured); ok && u.GroupVersionKind() == ruleGVK {
			return apierrors.NewNotFound(schema.GroupResource{Group: ruleGVK.Group, Resource: "devicetaintrules"}, key.Name)
		}
		return c.Get(ctx, key, o, opts...)
	}}
	f := newQuarantineFixture(t, quarantineOptions{devices: []quarantine.Device{quarGPU0}, funcs: hidden},
		newCluster(), replaceable(quarantine.RequestQuarantine))

	if err := f.reconcile(t); err == nil {
		t.Fatal("Reconcile succeeded; want the missing rule reported")
	}
	m := f.machine(t)
	if !m.DeletionTimestamp.IsZero() {
		t.Error("the Machine was deleted without its rule")
	}
	if got := m.Annotations[quarantine.ReplaceStatusAnnotation]; got != quarantine.StatusQuarantining {
		t.Errorf("status = %q; want %q", got, quarantine.StatusQuarantining)
	}
	assertEvents(t, f.events())
}

func TestQuarantineCarriesOnAfterAFailure(t *testing.T) {
	failures := map[string]bool{"create": true, "delete": true}
	funcs := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			if failures["create"] {
				failures["create"] = false
				return errors.New("connection reset")
			}
			return c.Create(ctx, o, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
			if failures["delete"] {
				failures["delete"] = false
				return errors.New("connection reset")
			}
			return c.Delete(ctx, o, opts...)
		},
	}
	f := newQuarantineFixture(t, quarantineOptions{devices: []quarantine.Device{quarGPU0}, funcs: funcs},
		newCluster(), replaceable(quarantine.RequestQuarantine))

	for _, want := range []string{quarantine.StatusQuarantining, quarantine.StatusReplacing} {
		if err := f.reconcile(t); err == nil {
			t.Fatalf("Reconcile succeeded; want the failure returned for a retry")
		}
		m := f.machine(t)
		if got := m.Annotations[quarantine.ReplaceStatusAnnotation]; got != want || !m.DeletionTimestamp.IsZero() {
			t.Fatalf("status = %q, deleted = %v; want %q and the Machine kept", got, !m.DeletionTimestamp.IsZero(), want)
		}
	}
	if err := f.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if f.machine(t).DeletionTimestamp.IsZero() {
		t.Error("the Machine was not deleted once the failures were over")
	}
	if got := len(f.rules(t)); got != 1 {
		t.Errorf("%d rules; want 1", got)
	}
	assertEvents(t, f.events(), EventDevicesQuarantined, EventMachineReplaced)
}

func TestQuarantineStartsOverWhenTheMachineChanges(t *testing.T) {
	for name, change := range map[string]func(*clusterv1.Machine){
		"the request is changed":   func(m *clusterv1.Machine) { m.Annotations[quarantine.ReplaceAnnotation] = "quarantine:gpu-0" },
		"the request is withdrawn": func(m *clusterv1.Machine) { delete(m.Annotations, quarantine.ReplaceAnnotation) },
		"the Machine is paused":    func(m *clusterv1.Machine) { m.Annotations[clusterv1.PausedAnnotation] = "" },
	} {
		t.Run(name, func(t *testing.T) {
			// Someone changes the Machine while its rules are made, after
			// the controller read it.
			changed := false
			funcs := interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
				if !changed {
					changed = true
					m := &clusterv1.Machine{}
					if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: quarantineMachine}, m); err != nil {
						return err
					}
					change(m)
					if err := c.Update(ctx, m); err != nil {
						return err
					}
				}
				return c.Create(ctx, o, opts...)
			}}
			f := newQuarantineFixture(t, quarantineOptions{devices: []quarantine.Device{quarGPU0}, funcs: funcs},
				newCluster(), replaceable(quarantine.RequestQuarantine))

			res, err := f.reconcileResult(t)
			if err != nil || res.RequeueAfter != conflictRetry {
				t.Fatalf("Reconcile = %+v, %v; want a retry after %s", res, err, conflictRetry)
			}
			if !f.machine(t).DeletionTimestamp.IsZero() {
				t.Fatal("the Machine was deleted although it changed under the request")
			}
			assertEvents(t, f.events())

			// Evaluated again, the Machine as it is now decides.
			if err := f.reconcile(t); err != nil {
				t.Fatalf("second Reconcile: %v", err)
			}
			m := f.machine(t)
			switch name {
			case "the request is changed":
				if m.DeletionTimestamp.IsZero() {
					t.Error("the changed request was not carried out")
				}
			case "the request is withdrawn":
				if !m.DeletionTimestamp.IsZero() || m.Annotations[quarantine.ReplaceStatusAnnotation] != "" {
					t.Errorf("a withdrawn request went on: deleted=%v annotations=%v", !m.DeletionTimestamp.IsZero(), m.Annotations)
				}
			case "the Machine is paused":
				if !m.DeletionTimestamp.IsZero() || m.Annotations[quarantine.ReplaceStatusAnnotation] != quarantine.RejectedStatus(quarantine.Reason(capi.SkipPaused)) {
					t.Errorf("a paused Machine went on: deleted=%v annotations=%v", !m.DeletionTimestamp.IsZero(), m.Annotations)
				}
			}
		})
	}
}

func TestQuarantineDoesNotActOnAStaleMachine(t *testing.T) {
	// The cache still shows the Machine as it was before the last status
	// patch, as it can right after one.
	f := newQuarantineFixture(t, quarantineOptions{devices: []quarantine.Device{quarGPU0}},
		newCluster(), replaceable(quarantine.RequestQuarantine))
	stale := f.machine(t)
	if err := f.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.events()

	reader := &staleReader{Client: f.hub, machine: stale}
	f.r.Client = reader
	stale.Annotations[quarantine.ReplaceStatusAnnotation] = quarantine.StatusReplacing
	res, err := f.reconcileResult(t)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != conflictRetry {
		t.Errorf("Reconcile = %+v; want a retry once the cache has caught up", res)
	}
	assertEvents(t, f.events())
}

// staleReader serves an old copy of the Machine and passes everything else
// through.
type staleReader struct {
	client.Client
	machine *clusterv1.Machine
}

func (s *staleReader) Get(ctx context.Context, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
	if m, ok := o.(*clusterv1.Machine); ok && key.Name == s.machine.Name {
		s.machine.DeepCopyInto(m)
		return nil
	}

	return s.Client.Get(ctx, key, o, opts...)
}

func TestQuarantineCarriesOutARequestEditedJustBeforeTheDeletion(t *testing.T) {
	// The operator corrects the request after every rule of the first one
	// is in place, just before the Machine is deleted.
	edited := false
	funcs := interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
		if !edited {
			edited = true
			m := &clusterv1.Machine{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: quarantineMachine}, m); err != nil {
				return err
			}
			m.Annotations[quarantine.ReplaceAnnotation] = "quarantine:gpu-1"
			if err := c.Update(ctx, m); err != nil {
				return err
			}
		}
		return c.Delete(ctx, o, opts...)
	}}
	f := newQuarantineFixture(t, quarantineOptions{devices: []quarantine.Device{quarGPU0, quarGPU1}, funcs: funcs},
		newCluster(), replaceable("quarantine:gpu-0"))

	if res, err := f.reconcileResult(t); err != nil || res.RequeueAfter != conflictRetry {
		t.Fatalf("Reconcile = %+v, %v; want a retry after %s", res, err, conflictRetry)
	}
	if !f.machine(t).DeletionTimestamp.IsZero() {
		t.Fatal("the Machine was deleted for the request as it was before the edit")
	}
	if err := f.reconcile(t); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if f.machine(t).DeletionTimestamp.IsZero() {
		t.Error("the Machine was not deleted for the edited request")
	}
	var got []string
	for _, r := range f.rules(t) {
		got = append(got, r.GetName())
	}
	want := []string{quarantine.RuleName(quarGPU0), quarantine.RuleName(quarGPU1)}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("rules = %v; want %v, the edited request's device among them", got, want)
	}
}

func TestQuarantineCarriedOutAgainKeepsItsStatus(t *testing.T) {
	// The device's rule was made for another Machine first, so it never
	// counts as this one's, and the deletion keeps failing. Each retry must
	// leave the status alone: a status patch would wake the controller
	// again ahead of the backoff.
	patches := 0
	funcs := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
			patches++
			return c.Patch(ctx, o, p, opts...)
		},
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return errors.New("connection reset")
		},
	}
	f := newQuarantineFixture(t, quarantineOptions{devices: []quarantine.Device{quarGPU0}, funcs: funcs},
		newCluster(), ruleOf(quarGPU0, "other"), replaceable("quarantine:gpu-0", func(m *clusterv1.Machine) {
			m.Annotations[quarantine.ReplaceStatusAnnotation] = quarantine.StatusReplacing
		}))

	for range 2 {
		if err := f.reconcile(t); err == nil {
			t.Fatal("Reconcile succeeded; want the failed deletion returned for a retry")
		}
	}
	if patches != 0 {
		t.Errorf("%d status patches; want none", patches)
	}
	if got := f.machine(t).Annotations[quarantine.ReplaceStatusAnnotation]; got != quarantine.StatusReplacing {
		t.Errorf("status = %q; want %q", got, quarantine.StatusReplacing)
	}
}
