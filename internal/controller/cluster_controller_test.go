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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/capi"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/decision"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal/extrr"
)

const (
	testNamespace = "tenant-a"
	testCluster   = "gpu"

	replaceMessage = "ErrorCode:DCGM_FR_XID_ERROR GPU_UUID:GPU-1234 xid 79 seen Recommended Action=REPLACE_VM;"
	restartMessage = "ErrorCode:GPU_FABRIC_DEGRADED Recommended Action=RESTART_BM;"
	reportMessage  = "ErrorCode:DCGM_FR_CLOCK_THROTTLE_THERMAL Recommended Action=CONTACT_SUPPORT;"
)

var clusterKey = client.ObjectKey{Namespace: testNamespace, Name: testCluster}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add cluster-api scheme: %v", err)
	}

	return scheme
}

func newCluster(mutate ...func(*clusterv1.Cluster)) *clusterv1.Cluster {
	c := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testCluster}}
	for _, f := range mutate {
		f(c)
	}

	return c
}

func newMachine(name, nodeName string, mutate ...func(*clusterv1.Machine)) *clusterv1.Machine {
	m := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace,
			Name:      name,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: testCluster},
		},
		Spec:   clusterv1.MachineSpec{ClusterName: testCluster},
		Status: clusterv1.MachineStatus{NodeRef: clusterv1.MachineNodeReference{Name: nodeName}},
	}
	for _, f := range mutate {
		f(m)
	}

	return m
}

func controlPlane(m *clusterv1.Machine) {
	m.Labels[clusterv1.MachineControlPlaneLabel] = ""
}

// defaultHealthCheck is the check every fixture has unless told otherwise:
// it covers every Machine of the Cluster and remediates by replacement,
// which is what a ClusterClass topology generates by default.
const defaultHealthCheck = "gpu-workers"

var rebootTemplate = clusterv1.MachineHealthCheckRemediationTemplateReference{
	APIVersion: "infrastructure.example.com/v1alpha1",
	Kind:       "RebootRemediationTemplate",
	Name:       "reboot",
}

func newHealthCheck(name string, mutate ...func(*clusterv1.MachineHealthCheck)) *clusterv1.MachineHealthCheck {
	mhc := &clusterv1.MachineHealthCheck{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name},
		Spec:       clusterv1.MachineHealthCheckSpec{ClusterName: testCluster},
	}
	for _, f := range mutate {
		f(mhc)
	}

	return mhc
}

func withTemplate(mhc *clusterv1.MachineHealthCheck) {
	mhc.Spec.Remediation.TemplateRef = rebootTemplate
}

func optedOut(m *clusterv1.Machine) {
	m.Annotations = map[string]string{clusterv1.MachineSkipRemediationAnnotation: ""}
}

func newNode(name string, conds ...corev1.NodeCondition) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Conditions: conds},
	}
}

func raised(check, message string) corev1.NodeCondition {
	return corev1.NodeCondition{Type: corev1.NodeConditionType(check), Status: corev1.ConditionTrue, Message: message}
}

// fixture wires a reconciler to a fake management cluster and one fake
// workload cluster.
type fixture struct {
	hub      client.Client
	workload client.Client
	recorder *events.FakeRecorder
	r        *ClusterReconciler
	// sourceEvents collects the SignalSourceSelected Events drained by
	// events, which every first poll records and most tests do not care
	// about.
	sourceEvents []string
}

type fixtureOptions struct {
	dryRun bool
	// healthChecks replace the default covering check; set uncovered to
	// have none at all.
	healthChecks    []*clusterv1.MachineHealthCheck
	uncovered       bool
	restartFallback RestartFallback
	// workloadMapper sets what the workload cluster serves; by default
	// nothing beyond the built-in kinds, so node conditions are read.
	workloadMapper meta.RESTMapper
	// workloadObjs are added to the workload cluster next to the nodes.
	workloadObjs  []client.Object
	disconnected  bool
	selector      labels.Selector
	hubFuncs      interceptor.Funcs
	workloadFuncs interceptor.Funcs
}

func newFixture(t *testing.T, opts fixtureOptions, hubObjs []client.Object, nodes ...*corev1.Node) *fixture {
	t.Helper()

	scheme := newScheme(t)
	checks := opts.healthChecks
	if checks == nil && !opts.uncovered {
		checks = []*clusterv1.MachineHealthCheck{newHealthCheck(defaultHealthCheck)}
	}
	for _, mhc := range checks {
		hubObjs = append(hubObjs, mhc)
	}
	hub := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hubObjs...).WithInterceptorFuncs(opts.hubFuncs).Build()

	workloadObjs := append([]client.Object{}, opts.workloadObjs...)
	for _, n := range nodes {
		workloadObjs = append(workloadObjs, n)
	}
	var requests []client.Object
	for _, o := range opts.workloadObjs {
		if _, ok := o.(*unstructured.Unstructured); ok {
			requests = append(requests, o)
		}
	}
	workloadBuilder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workloadObjs...).
		WithStatusSubresource(requests...).WithInterceptorFuncs(opts.workloadFuncs)
	if opts.workloadMapper != nil {
		workloadBuilder = workloadBuilder.WithRESTMapper(opts.workloadMapper)
	}
	workload := workloadBuilder.Build()

	cache := clustercache.NewFakeClusterCache(workload, clusterKey)
	if opts.disconnected {
		cache = clustercache.NewFakeEmptyClusterCache()
	}

	recorder := events.NewFakeRecorder(32)

	return &fixture{
		hub:      hub,
		workload: workload,
		recorder: recorder,
		r: &ClusterReconciler{
			Client:          hub,
			ClusterCache:    cache,
			Recorder:        recorder,
			Table:           decision.Default(),
			DryRun:          opts.dryRun,
			PollInterval:    time.Minute,
			ClusterSelector: opts.selector,
			RestartFallback: opts.restartFallback,
		},
	}
}

func (f *fixture) reconcile(t *testing.T) ctrl.Result {
	t.Helper()

	res, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: clusterKey})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	return res
}

// events drains the Events recorded so far, except SignalSourceSelected
// ones, which it keeps in sourceEvents.
func (f *fixture) events() []string {
	var out []string
	for {
		select {
		case e := <-f.recorder.Events:
			if strings.Contains(e, " "+EventSignalSourceSelected+" ") {
				f.sourceEvents = append(f.sourceEvents, e)
				continue
			}
			out = append(out, e)
		default:
			return out
		}
	}
}

func (f *fixture) machine(t *testing.T, name string) *clusterv1.Machine {
	t.Helper()

	m := &clusterv1.Machine{}
	if err := f.hub.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, m); err != nil {
		t.Fatalf("get machine %s: %v", name, err)
	}

	return m
}

func (f *fixture) observed(key string) (observation, bool) {
	obs, ok := f.r.observations(clusterKey)[key]
	return obs, ok
}

func assertEvents(t *testing.T, got []string, want ...string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	for i := range want {
		if !strings.Contains(got[i], want[i]) {
			t.Fatalf("event %d = %q, want it to contain %q", i, got[i], want[i])
		}
	}
}

func assertPoll(t *testing.T, res ctrl.Result) {
	t.Helper()

	if res.RequeueAfter != time.Minute {
		t.Fatalf("result = %+v, want a requeue after the poll interval", res)
	}
}

func TestReconcileDryRunOnlyLogs(t *testing.T) {
	f := newFixture(t, fixtureOptions{dryRun: true},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")},
		newNode("gpu-w-1", raised("GpuXidError", replaceMessage)))

	for range 2 {
		assertPoll(t, f.reconcile(t))
	}

	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("dry run marked the Machine")
	}
	assertEvents(t, f.events())

	obs, ok := f.observed("gpu-w-1/GpuXidError")
	if !ok || obs.Decision != decision.Replace || obs.Machine != "gpu-w-1" || obs.Skip != "" {
		t.Fatalf("observation = %+v, %v; want a Replace on gpu-w-1", obs, ok)
	}
}

func TestReconcileMarksMachineForRemediation(t *testing.T) {
	f := newFixture(t, fixtureOptions{},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1"), newMachine("gpu-w-2", "gpu-w-2")},
		newNode("gpu-w-1", raised("GpuXidError", replaceMessage)),
		newNode("gpu-w-2"))

	assertPoll(t, f.reconcile(t))

	m := f.machine(t, "gpu-w-1")
	if !capi.IsMarkedForRemediation(m) {
		t.Fatal("Machine not marked")
	}
	wantReason := "NodeCondition GpuXidError DCGM_FR_XID_ERROR gpu GPU-1234 (REPLACE_VM): REPLACE_VM maps to Replace; " +
		"MachineHealthCheck gpu-workers remediates by replacement"
	if got := m.Annotations[capi.RemediationReasonAnnotation]; got != wantReason {
		t.Fatalf("reason annotation = %q, want %q", got, wantReason)
	}
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-2")) {
		t.Fatal("healthy Machine was marked")
	}
	assertEvents(t, f.events(), "Warning MarkedForRemediation Marked for remediation: "+wantReason)

	// The signal persists: the Machine is already marked, which is
	// remediation in progress and not news.
	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events())
	if obs, _ := f.observed("gpu-w-1/GpuXidError"); obs.Skip != capi.SkipAlreadyMarked {
		t.Fatalf("second observation = %+v, want skip %q", obs, capi.SkipAlreadyMarked)
	}
}

func TestReconcileReportsOnceUntilTheSignalClears(t *testing.T) {
	node := newNode("gpu-w-1", raised("GpuThermal", reportMessage))
	f := newFixture(t, fixtureOptions{},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")}, node)

	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events(),
		"Warning NodeHealthReported GpuThermal DCGM_FR_CLOCK_THROTTLE_THERMAL (CONTACT_SUPPORT) on node gpu-w-1: CONTACT_SUPPORT maps to Report")
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("a Report decision marked the Machine")
	}

	// Still raised: nothing new to say.
	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events())

	// NVSentinel lowers the condition once its check passes again.
	node.Status.Conditions[0].Status = corev1.ConditionFalse
	if err := f.workload.Status().Update(context.Background(), node); err != nil {
		t.Fatalf("update node: %v", err)
	}
	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events())
	if _, ok := f.observed("gpu-w-1/GpuThermal"); ok {
		t.Fatal("cleared signal still remembered")
	}

	// Raised again later: that is news again.
	node.Status.Conditions[0].Status = corev1.ConditionTrue
	if err := f.workload.Status().Update(context.Background(), node); err != nil {
		t.Fatalf("update node: %v", err)
	}
	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events(), "Warning NodeHealthReported GpuThermal")
}

func TestReconcileEscalationIsNews(t *testing.T) {
	node := newNode("gpu-w-1", raised("GpuXidError", "ErrorCode:DCGM_FR_XID_ERROR Recommended Action=RESTART_BM;"))
	f := newFixture(t, fixtureOptions{},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")}, node)

	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events(), "Warning RestartUnavailable GpuXidError DCGM_FR_XID_ERROR (RESTART_BM) calls for a restart of node gpu-w-1, "+
		"but MachineHealthCheck gpu-workers remediates by replacement; reported only")
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("a Restart decision marked the Machine")
	}

	// A second event lands on the same condition and the recommendation
	// escalates: the decision changes, so it is acted on and recorded.
	node.Status.Conditions[0].Message = "ErrorCode:DCGM_FR_XID_ERROR Recommended Action=RESTART_BM;ErrorCode:DCGM_FR_XID_ERROR Recommended Action=REPLACE_VM;"
	if err := f.workload.Status().Update(context.Background(), node); err != nil {
		t.Fatalf("update node: %v", err)
	}
	assertPoll(t, f.reconcile(t))
	if !capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("escalated signal did not mark the Machine")
	}
	assertEvents(t, f.events(), "Warning MarkedForRemediation")
}

func TestReconcileNeverEscalatesATruncatedSignal(t *testing.T) {
	// The connector cut the condition message, so the events that would
	// have called for a replacement may be missing: the decision stays with
	// what is visible and says so.
	f := newFixture(t, fixtureOptions{},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")},
		newNode("gpu-w-1", raised("GpuXidError", "ErrorCode:DCGM_FR_XID_ERROR Recommended Action=RESTART_BM;...")))

	assertPoll(t, f.reconcile(t))
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("truncated signal marked the Machine")
	}
	assertEvents(t, f.events(), "Warning RestartUnavailable GpuXidError DCGM_FR_XID_ERROR (RESTART_BM) [truncated] calls for a restart")
	if obs, _ := f.observed("gpu-w-1/GpuXidError"); !obs.Truncated {
		t.Fatalf("observation = %+v, want truncated", obs)
	}
}

func TestReconcileSkipsControlPlaneMachinesOnce(t *testing.T) {
	f := newFixture(t, fixtureOptions{},
		[]client.Object{newCluster(), newMachine("gpu-cp-1", "gpu-cp-1", controlPlane)},
		newNode("gpu-cp-1", raised("GpuXidError", replaceMessage)))

	assertPoll(t, f.reconcile(t))
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-cp-1")) {
		t.Fatal("control plane Machine was marked")
	}
	assertEvents(t, f.events(), "Warning RemediationSkipped Remediation skipped because the Machine is a control plane member: NodeCondition GpuXidError")

	for range 2 {
		assertPoll(t, f.reconcile(t))
		assertEvents(t, f.events())
	}
}

func TestReconcileReportsNodesWithoutMachineOnTheCluster(t *testing.T) {
	f := newFixture(t, fixtureOptions{},
		[]client.Object{newCluster()},
		newNode("stray", raised("GpuXidError", replaceMessage)))

	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events(), "Warning MachineNotFound node stray reports GpuXidError DCGM_FR_XID_ERROR gpu GPU-1234 (REPLACE_VM) but no Machine owns it")

	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events())
}

func TestReconcileLeavesPausedClustersAlone(t *testing.T) {
	paused := true
	tests := []struct {
		name   string
		mutate func(*clusterv1.Cluster)
	}{
		{"spec.paused", func(c *clusterv1.Cluster) { c.Spec.Paused = &paused }},
		{"paused annotation", func(c *clusterv1.Cluster) {
			c.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, fixtureOptions{},
				[]client.Object{newCluster(tt.mutate), newMachine("gpu-w-1", "gpu-w-1")},
				newNode("gpu-w-1", raised("GpuXidError", replaceMessage)))

			// Only spec.paused transitions are watched, so a paused Cluster
			// keeps being polled to notice the annotation going away.
			assertPoll(t, f.reconcile(t))
			if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
				t.Fatal("paused Cluster's Machine was marked")
			}
			assertEvents(t, f.events())
		})
	}
}

func TestReconcileWaitsForDisconnectedClusters(t *testing.T) {
	f := newFixture(t, fixtureOptions{disconnected: true},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")})

	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events())
}

func TestReconcileRetriesWhenCollectingFails(t *testing.T) {
	f := newFixture(t, fixtureOptions{workloadFuncs: interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("apiserver unreachable")
		},
	}}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")})

	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events())
}

func TestReconcileRetriesAFailedMark(t *testing.T) {
	f := newFixture(t, fixtureOptions{hubFuncs: interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return errors.New("webhook denied the patch")
		},
	}}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")},
		newNode("gpu-w-1", raised("GpuXidError", replaceMessage)))

	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events())
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("Machine marked despite the failed patch")
	}
	// Nothing was concluded, so the next poll treats the signal as new again.
	if obs, _ := f.observed("gpu-w-1/GpuXidError"); obs != (observation{}) {
		t.Fatalf("observation after a failed mark = %+v, want none", obs)
	}
}

func TestReconcileForgetsClustersThatGoAway(t *testing.T) {
	cluster := newCluster()
	f := newFixture(t, fixtureOptions{},
		[]client.Object{cluster, newMachine("gpu-w-1", "gpu-w-1")},
		newNode("gpu-w-1", raised("GpuThermal", reportMessage)))

	assertPoll(t, f.reconcile(t))
	if _, ok := f.observed("gpu-w-1/GpuThermal"); !ok {
		t.Fatal("signal not remembered")
	}

	// A finalizer keeps the Cluster around with a deletion timestamp first,
	// as Cluster API's own finalizer does; then it is gone for good.
	cluster.Finalizers = []string{clusterv1.ClusterFinalizer}
	if err := f.hub.Update(context.Background(), cluster); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	if err := f.hub.Delete(context.Background(), cluster); err != nil {
		t.Fatalf("delete cluster: %v", err)
	}
	if res := f.reconcile(t); res != (ctrl.Result{}) {
		t.Fatalf("result for a deleting Cluster = %+v, want none", res)
	}
	if _, ok := f.observed("gpu-w-1/GpuThermal"); ok {
		t.Fatal("deleting Cluster still remembered")
	}

	// Releasing the finalizer lets the fake client, like the API server,
	// remove the object for good.
	if err := f.hub.Get(context.Background(), clusterKey, cluster); err != nil {
		t.Fatalf("get deleting cluster: %v", err)
	}
	cluster.Finalizers = nil
	if err := f.hub.Update(context.Background(), cluster); err != nil {
		t.Fatalf("remove finalizer: %v", err)
	}
	if res := f.reconcile(t); res != (ctrl.Result{}) {
		t.Fatalf("result for a deleted Cluster = %+v, want none", res)
	}
}

func TestReconcileHonoursTheClusterSelector(t *testing.T) {
	selector, err := labels.Parse("environment=gpu")
	if err != nil {
		t.Fatal(err)
	}

	unselected := newFixture(t, fixtureOptions{selector: selector},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")},
		newNode("gpu-w-1", raised("GpuXidError", replaceMessage)))
	if res := unselected.reconcile(t); res != (ctrl.Result{}) {
		t.Fatalf("result for an unselected Cluster = %+v, want none", res)
	}
	if capi.IsMarkedForRemediation(unselected.machine(t, "gpu-w-1")) {
		t.Fatal("unselected Cluster's Machine was marked")
	}

	selected := newFixture(t, fixtureOptions{selector: selector},
		[]client.Object{newCluster(func(c *clusterv1.Cluster) { c.Labels = map[string]string{"environment": "gpu"} }),
			newMachine("gpu-w-1", "gpu-w-1")},
		newNode("gpu-w-1", raised("GpuXidError", replaceMessage)))
	assertPoll(t, selected.reconcile(t))
	if !capi.IsMarkedForRemediation(selected.machine(t, "gpu-w-1")) {
		t.Fatal("selected Cluster's Machine was not marked")
	}
}

func TestReconcileRestartsThroughRemediationTemplates(t *testing.T) {
	f := newFixture(t, fixtureOptions{healthChecks: []*clusterv1.MachineHealthCheck{newHealthCheck("reboot", withTemplate)}},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")},
		newNode("gpu-w-1", raised("GpuXidError", restartMessage)))

	assertPoll(t, f.reconcile(t))

	m := f.machine(t, "gpu-w-1")
	if !capi.IsMarkedForRemediation(m) {
		t.Fatal("Restart covered by a remediation template did not mark the Machine")
	}
	wantReason := "NodeCondition GpuXidError GPU_FABRIC_DEGRADED (RESTART_BM): RESTART_BM maps to Restart; " +
		"MachineHealthCheck reboot remediates through RebootRemediationTemplate reboot"
	if got := m.Annotations[capi.RemediationReasonAnnotation]; got != wantReason {
		t.Fatalf("reason annotation = %q, want %q", got, wantReason)
	}
	assertEvents(t, f.events(), "Warning MarkedForRemediation Marked for remediation: "+wantReason)

	// Cluster API now owns the request; the signal persisting is not news.
	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events())
}

// TestReconcileFollowsTheDecisionTable pins that the table the reconciler is
// given decides, not the built-in one: an action the defaults report is
// acted on once an entry says so, and one they act on is left alone.
func TestReconcileFollowsTheDecisionTable(t *testing.T) {
	table, err := decision.New(map[string]string{"CONTACT_SUPPORT": "restart", "RESTART_BM": "report"})
	if err != nil {
		t.Fatal(err)
	}

	f := newFixture(t, fixtureOptions{healthChecks: []*clusterv1.MachineHealthCheck{newHealthCheck("reboot", withTemplate)}},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1"), newMachine("gpu-w-2", "gpu-w-2")},
		newNode("gpu-w-1", raised("GpuThermal", reportMessage)),
		newNode("gpu-w-2", raised("GpuXidError", restartMessage)))
	f.r.Table = table

	assertPoll(t, f.reconcile(t))

	m := f.machine(t, "gpu-w-1")
	if !capi.IsMarkedForRemediation(m) {
		t.Fatal("an action mapped to Restart did not mark the Machine")
	}
	if got := m.Annotations[capi.RemediationReasonAnnotation]; !strings.Contains(got, "CONTACT_SUPPORT maps to Restart") {
		t.Fatalf("reason annotation = %q, want it to name the entry", got)
	}
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-2")) {
		t.Fatal("an action mapped to Report marked the Machine")
	}
}

func TestReconcileReportsRestartsNoTemplateCanCarryOut(t *testing.T) {
	tests := []struct {
		name   string
		opts   fixtureOptions
		reason string
	}{
		{"no check", fixtureOptions{uncovered: true}, "no MachineHealthCheck selects the Machine"},
		{"replacement only", fixtureOptions{}, "MachineHealthCheck gpu-workers remediates by replacement"},
		// A check without a template would replace the Machine alongside
		// the one restarting it.
		{"mixed", fixtureOptions{healthChecks: []*clusterv1.MachineHealthCheck{
			newHealthCheck("reboot", withTemplate), newHealthCheck("replace"),
		}}, "MachineHealthCheck reboot remediates through RebootRemediationTemplate reboot; " +
			"MachineHealthCheck replace remediates by replacement"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.opts,
				[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")},
				newNode("gpu-w-1", raised("GpuXidError", restartMessage)))

			assertPoll(t, f.reconcile(t))
			if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
				t.Fatal("Restart marked the Machine without a template to carry it out")
			}
			assertEvents(t, f.events(),
				"Warning RestartUnavailable GpuXidError GPU_FABRIC_DEGRADED (RESTART_BM) calls for a restart of node gpu-w-1, but "+
					tt.reason+"; reported only")

			assertPoll(t, f.reconcile(t))
			assertEvents(t, f.events())
		})
	}
}

func TestReconcileReportsReplacementsWithoutHealthCheck(t *testing.T) {
	f := newFixture(t, fixtureOptions{uncovered: true},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")},
		newNode("gpu-w-1", raised("GpuXidError", replaceMessage)))

	assertPoll(t, f.reconcile(t))
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("Replace marked a Machine that no MachineHealthCheck selects")
	}
	assertEvents(t, f.events(), "Warning ReplaceUnavailable GpuXidError DCGM_FR_XID_ERROR gpu GPU-1234 (REPLACE_VM) "+
		"calls for a replacement of node gpu-w-1, but no MachineHealthCheck selects the Machine; reported only")

	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events())

	// A check appearing changes what marking does: that is news, and acted on.
	if err := f.hub.Create(context.Background(), newHealthCheck(defaultHealthCheck)); err != nil {
		t.Fatalf("create machinehealthcheck: %v", err)
	}
	assertPoll(t, f.reconcile(t))
	if !capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("Machine not marked once a MachineHealthCheck selects it")
	}
	assertEvents(t, f.events(), "Warning MarkedForRemediation")
}

func TestReconcileReportsReplacementsWhileTheHealthCheckIsPaused(t *testing.T) {
	// A paused check would act on the annotation at some arbitrary time
	// after it is unpaused; the signal is reported until then.
	f := newFixture(t, fixtureOptions{healthChecks: []*clusterv1.MachineHealthCheck{
		newHealthCheck(defaultHealthCheck, func(mhc *clusterv1.MachineHealthCheck) {
			mhc.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
		}),
	}}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")},
		newNode("gpu-w-1", raised("GpuXidError", replaceMessage)))

	assertPoll(t, f.reconcile(t))
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("Machine marked while its only MachineHealthCheck is paused")
	}
	assertEvents(t, f.events(), "Warning ReplaceUnavailable GpuXidError DCGM_FR_XID_ERROR gpu GPU-1234 (REPLACE_VM) "+
		"calls for a replacement of node gpu-w-1, but MachineHealthCheck gpu-workers remediates by replacement (paused); waiting for the paused checks")

	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events())
}

func TestReconcileReplacesThroughTemplatesFirst(t *testing.T) {
	// A Replace on a Machine whose check remediates through a template gets
	// the template first; the reason records that.
	f := newFixture(t, fixtureOptions{healthChecks: []*clusterv1.MachineHealthCheck{newHealthCheck("reboot", withTemplate)}},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")},
		newNode("gpu-w-1", raised("GpuXidError", replaceMessage)))

	assertPoll(t, f.reconcile(t))
	m := f.machine(t, "gpu-w-1")
	if !capi.IsMarkedForRemediation(m) {
		t.Fatal("Machine not marked")
	}
	if got := m.Annotations[capi.RemediationReasonAnnotation]; !strings.HasSuffix(got,
		"; MachineHealthCheck reboot remediates through RebootRemediationTemplate reboot") {
		t.Fatalf("reason annotation = %q, want it to name the template", got)
	}
}

func TestReconcileDryRunRestartOnlyLogs(t *testing.T) {
	f := newFixture(t, fixtureOptions{dryRun: true, healthChecks: []*clusterv1.MachineHealthCheck{newHealthCheck("reboot", withTemplate)}},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")},
		newNode("gpu-w-1", raised("GpuXidError", restartMessage)))

	assertPoll(t, f.reconcile(t))
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("dry run marked the Machine")
	}
	assertEvents(t, f.events())

	obs, _ := f.observed("gpu-w-1/GpuXidError")
	if obs.Decision != decision.Restart || obs.Skip != "" ||
		obs.Coverage != "MachineHealthCheck reboot remediates through RebootRemediationTemplate reboot" {
		t.Fatalf("observation = %+v, want a Restart through the template", obs)
	}
}

func TestReconcileSkipsMachinesOptedOutOfRemediation(t *testing.T) {
	f := newFixture(t, fixtureOptions{},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1", optedOut)},
		newNode("gpu-w-1", raised("GpuXidError", replaceMessage)))

	assertPoll(t, f.reconcile(t))
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("opted-out Machine was marked")
	}
	assertEvents(t, f.events(), "Warning RemediationSkipped Remediation skipped because the Machine has opted out of remediation")

	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events())
}

func TestParseRestartFallback(t *testing.T) {
	for _, in := range []string{"report", "replace"} {
		if got, err := ParseRestartFallback(in); err != nil || string(got) != in {
			t.Errorf("ParseRestartFallback(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "Replace", "delete"} {
		if _, err := ParseRestartFallback(in); err == nil {
			t.Errorf("ParseRestartFallback(%q) accepted", in)
		}
	}
}

func withRestartFallback(value string) func(*clusterv1.Cluster) {
	return func(c *clusterv1.Cluster) {
		c.Annotations = map[string]string{RestartFallbackAnnotation: value}
	}
}

func TestReconcileRestartFallback(t *testing.T) {
	const fallbackReason = "NodeCondition GpuXidError GPU_FABRIC_DEGRADED (RESTART_BM): RESTART_BM maps to Restart, " +
		"which no remediation template can carry out; falling back to a replacement; " +
		"MachineHealthCheck gpu-workers remediates by replacement"

	tests := []struct {
		name     string
		global   RestartFallback
		cluster  *clusterv1.Cluster
		opts     fixtureOptions
		replaced bool
		event    string
	}{
		{"default reports", "", newCluster(), fixtureOptions{}, false,
			"Warning RestartUnavailable GpuXidError GPU_FABRIC_DEGRADED (RESTART_BM) calls for a restart of node gpu-w-1, " +
				"but MachineHealthCheck gpu-workers remediates by replacement; reported only (restart fallback report)"},
		{"replace", RestartFallbackReplace, newCluster(), fixtureOptions{}, true,
			"Warning MarkedForRemediation Marked for remediation: " + fallbackReason},
		{"replace needs a health check too", RestartFallbackReplace, newCluster(), fixtureOptions{uncovered: true}, false,
			"Warning RestartUnavailable GpuXidError GPU_FABRIC_DEGRADED (RESTART_BM) calls for a restart of node gpu-w-1, " +
				"but no MachineHealthCheck selects the Machine; reported only (restart fallback replace)"},
		{"cluster opts into replace", RestartFallbackReport, newCluster(withRestartFallback("replace")), fixtureOptions{}, true,
			"Warning MarkedForRemediation Marked for remediation: " + fallbackReason},
		{"cluster opts out of replace", RestartFallbackReplace, newCluster(withRestartFallback("report")), fixtureOptions{}, false,
			"Warning RestartUnavailable"},
		{"invalid override is ignored and said so", RestartFallbackReport, newCluster(withRestartFallback("reboot")), fixtureOptions{}, false,
			"reported only (restart fallback report; the Cluster's nvsentinel.petasus.io/restart-fallback annotation is ignored: " +
				`restart fallback "reboot" is neither "report" nor "replace")`},
		// A typo meant to opt out must show where it matters most: on the
		// replacement it failed to prevent.
		{"invalid override under replace is said so", RestartFallbackReplace, newCluster(withRestartFallback("Report")), fixtureOptions{}, true,
			"falling back to a replacement; the Cluster's nvsentinel.petasus.io/restart-fallback annotation is ignored: " +
				`restart fallback "Report" is neither "report" nor "replace"; MachineHealthCheck gpu-workers remediates by replacement`},
		{"replace with mixed checks", RestartFallbackReplace, newCluster(), fixtureOptions{healthChecks: []*clusterv1.MachineHealthCheck{
			newHealthCheck("reboot", withTemplate), newHealthCheck("replace"),
		}}, true, "falling back to a replacement; MachineHealthCheck reboot remediates through RebootRemediationTemplate reboot; " +
			"MachineHealthCheck replace remediates by replacement"},
		{"replace with only paused checks", RestartFallbackReplace, newCluster(), fixtureOptions{healthChecks: []*clusterv1.MachineHealthCheck{
			newHealthCheck(defaultHealthCheck, func(mhc *clusterv1.MachineHealthCheck) {
				mhc.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
			}),
		}}, false, "but MachineHealthCheck gpu-workers remediates by replacement (paused); waiting for the paused checks (restart fallback replace)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := tt.opts
			opts.restartFallback = tt.global
			node := newNode("gpu-w-1", raised("GpuXidError", restartMessage))
			f := newFixture(t, opts, []client.Object{tt.cluster, newMachine("gpu-w-1", "gpu-w-1")}, node)

			assertPoll(t, f.reconcile(t))
			action, marked := capi.MarkedAction(f.machine(t, "gpu-w-1"))
			if marked != tt.replaced || (marked && action != capi.ActionRemediate) {
				t.Fatalf("MarkedAction = %q, %v; want marked for a replacement: %v", action, marked, tt.replaced)
			}
			assertEvents(t, f.events(), tt.event)

			assertPoll(t, f.reconcile(t))
			assertEvents(t, f.events())

			// A replacement in place of a restart is never released.
			lower(t, f, node)
			assertPoll(t, f.reconcile(t))
			if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) != tt.replaced {
				t.Fatal("the fallback replacement was released")
			}
		})
	}
}

func TestReconcileRestartFallbackEscalatesAnEarlierRestartMark(t *testing.T) {
	// The Machine was marked for a restart while every check had a
	// template; a check without one appeared since. Under the replace
	// fallback the mark becomes a replacement mark, which is never released.
	node := newNode("gpu-w-1", raised("GpuXidError", restartMessage))
	f := newFixture(t, fixtureOptions{restartFallback: RestartFallbackReplace},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1", markedFor(capi.ActionRestart))}, node)

	assertPoll(t, f.reconcile(t))
	if action, _ := capi.MarkedAction(f.machine(t, "gpu-w-1")); action != capi.ActionRemediate {
		t.Fatalf("MarkedAction = %q, want the restart escalated to %q", action, capi.ActionRemediate)
	}
	assertEvents(t, f.events(), "Warning MarkedForRemediation Restart escalated to a replacement")

	lower(t, f, node)
	assertPoll(t, f.reconcile(t))
	if !capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("escalated Machine was released")
	}
}

func TestReconcileRestartFallbackChangeIsNews(t *testing.T) {
	cluster := newCluster()
	f := newFixture(t, fixtureOptions{uncovered: true}, []client.Object{cluster, newMachine("gpu-w-1", "gpu-w-1")},
		newNode("gpu-w-1", raised("GpuXidError", restartMessage)))

	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events(), "(restart fallback report)")

	cluster.Annotations = map[string]string{RestartFallbackAnnotation: "replace"}
	if err := f.hub.Update(context.Background(), cluster); err != nil {
		t.Fatalf("update cluster: %v", err)
	}
	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events(), "(restart fallback replace)")
}

func TestReconcileDryRunRestartFallback(t *testing.T) {
	f := newFixture(t, fixtureOptions{dryRun: true, restartFallback: RestartFallbackReplace},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")},
		newNode("gpu-w-1", raised("GpuXidError", restartMessage)))

	assertPoll(t, f.reconcile(t))
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("dry run marked the Machine")
	}
	assertEvents(t, f.events())
	if obs, _ := f.observed("gpu-w-1/GpuXidError"); obs.Fallback != "replace" || obs.Skip != "" {
		t.Fatalf("observation = %+v, want the replace fallback", obs)
	}
}

// markedFor marks the Machine the way the actuator does.
func markedFor(action string) func(*clusterv1.Machine) {
	return func(m *clusterv1.Machine) {
		m.Annotations = map[string]string{
			clusterv1.RemediateMachineAnnotation: "",
			capi.RemediationReasonAnnotation:     "earlier signal",
			capi.RemediationActionAnnotation:     action,
		}
	}
}

func lower(t *testing.T, f *fixture, node *corev1.Node) {
	t.Helper()

	node.Status.Conditions[0].Status = corev1.ConditionFalse
	if err := f.workload.Status().Update(context.Background(), node); err != nil {
		t.Fatalf("update node: %v", err)
	}
}

func TestReconcileReleasesARestartOnceTheSignalClears(t *testing.T) {
	node := newNode("gpu-w-1", raised("GpuXidError", restartMessage))
	f := newFixture(t, fixtureOptions{healthChecks: []*clusterv1.MachineHealthCheck{newHealthCheck("reboot", withTemplate)}},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")}, node)

	assertPoll(t, f.reconcile(t))
	if action, ok := capi.MarkedAction(f.machine(t, "gpu-w-1")); !ok || action != capi.ActionRestart {
		t.Fatalf("MarkedAction = %q, %v; want the Machine marked for a restart", action, ok)
	}
	assertEvents(t, f.events(), "Warning MarkedForRemediation")

	// The restart worked and NVSentinel's check passes again.
	lower(t, f, node)
	assertPoll(t, f.reconcile(t))
	m := f.machine(t, "gpu-w-1")
	if capi.IsMarkedForRemediation(m) {
		t.Fatal("Machine still marked after its restart signal cleared")
	}
	if _, ok := m.Annotations[capi.RemediationActionAnnotation]; ok {
		t.Fatal("action annotation left behind")
	}
	assertEvents(t, f.events(),
		"Normal RemediationReleased Released from remediation: no NodeCondition signal this operator acts on calls for a restart or a replacement of node gpu-w-1 any more")

	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events())
}

func TestReconcileReleasesOnlyItsOwnRestarts(t *testing.T) {
	// No node reports anything, so any Machine this operator marked for a
	// restart would be released.
	f := newFixture(t, fixtureOptions{},
		[]client.Object{newCluster(),
			newMachine("replace", "replace", markedFor(capi.ActionRemediate)),
			newMachine("someone-else", "someone-else", func(m *clusterv1.Machine) {
				m.Annotations = map[string]string{clusterv1.RemediateMachineAnnotation: ""}
			}),
			newMachine("restart", "restart", markedFor(capi.ActionRestart)),
		},
		newNode("replace"), newNode("someone-else"), newNode("restart"))

	assertPoll(t, f.reconcile(t))
	for _, name := range []string{"replace", "someone-else"} {
		if !capi.IsMarkedForRemediation(f.machine(t, name)) {
			t.Fatalf("%s was released", name)
		}
	}
	if capi.IsMarkedForRemediation(f.machine(t, "restart")) {
		t.Fatal("this operator's restart was not released")
	}
	assertEvents(t, f.events(), "Normal RemediationReleased")
}

func TestReconcileKeepsARestartWhileASignalCallsForRemediation(t *testing.T) {
	tests := []struct {
		name    string
		message string
		release bool
	}{
		{"restart", restartMessage, false},
		// An escalation keeps the Machine marked for Cluster API to replace.
		{"escalated to a replacement", replaceMessage, false},
		// A signal that is only reported does not hold the Machine.
		{"report only", reportMessage, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, fixtureOptions{healthChecks: []*clusterv1.MachineHealthCheck{newHealthCheck("reboot", withTemplate)}},
				[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1", markedFor(capi.ActionRestart))},
				newNode("gpu-w-1", raised("GpuXidError", tt.message)))

			assertPoll(t, f.reconcile(t))
			if released := !capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")); released != tt.release {
				t.Fatalf("released = %v, want %v", released, tt.release)
			}
		})
	}
}

func TestReconcileNeverReleasesARestartEscalatedToAReplacement(t *testing.T) {
	node := newNode("gpu-w-1", raised("GpuXidError", replaceMessage))
	f := newFixture(t, fixtureOptions{healthChecks: []*clusterv1.MachineHealthCheck{newHealthCheck("reboot", withTemplate)}},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1", markedFor(capi.ActionRestart))}, node)

	assertPoll(t, f.reconcile(t))
	if action, _ := capi.MarkedAction(f.machine(t, "gpu-w-1")); action != capi.ActionRemediate {
		t.Fatalf("MarkedAction = %q, want the restart escalated to %q", action, capi.ActionRemediate)
	}
	assertEvents(t, f.events(), "Warning MarkedForRemediation Restart escalated to a replacement: NodeCondition GpuXidError")

	// The provider's restart lowers the signal; the replacement still stands.
	lower(t, f, node)
	assertPoll(t, f.reconcile(t))
	if !capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("escalated Machine was released")
	}
	assertEvents(t, f.events())
}

func TestReconcileRetriesAFailedEscalation(t *testing.T) {
	failing := true
	f := newFixture(t, fixtureOptions{hubFuncs: interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if failing {
				return errors.New("webhook denied the patch")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1", markedFor(capi.ActionRestart))},
		newNode("gpu-w-1", raised("GpuXidError", replaceMessage)))

	assertPoll(t, f.reconcile(t))
	if action, _ := capi.MarkedAction(f.machine(t, "gpu-w-1")); action != capi.ActionRestart {
		t.Fatalf("MarkedAction = %q after a failed escalation", action)
	}
	if obs, _ := f.observed("gpu-w-1/GpuXidError"); obs != (observation{}) {
		t.Fatalf("observation after a failed escalation = %+v, want none", obs)
	}
	assertEvents(t, f.events())

	failing = false
	assertPoll(t, f.reconcile(t))
	if action, _ := capi.MarkedAction(f.machine(t, "gpu-w-1")); action != capi.ActionRemediate {
		t.Fatalf("MarkedAction = %q, want the escalation retried", action)
	}
	assertEvents(t, f.events(), "Warning MarkedForRemediation Restart escalated to a replacement")
}

func TestReconcileReleasesNothingWhenSignalsCannotBeRead(t *testing.T) {
	f := newFixture(t, fixtureOptions{workloadFuncs: interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("apiserver unreachable")
		},
	}}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1", markedFor(capi.ActionRestart))})

	assertPoll(t, f.reconcile(t))
	if !capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("Machine released although its node could not be read")
	}
	assertEvents(t, f.events())
}

func TestReconcileDryRunReleasesNothing(t *testing.T) {
	f := newFixture(t, fixtureOptions{dryRun: true},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1", markedFor(capi.ActionRestart))},
		newNode("gpu-w-1"))

	for range 2 {
		assertPoll(t, f.reconcile(t))
		if !capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
			t.Fatal("dry run released the Machine")
		}
		assertEvents(t, f.events())
		// Remembered, so the second poll does not say it again.
		if !f.r.releasableBefore(clusterKey, "gpu-w-1") {
			t.Fatal("dry run did not remember the Machine it would release")
		}
	}

	// The Cluster goes away: nothing is remembered about it any more.
	f.r.forget(clusterKey)
	if f.r.releasableBefore(clusterKey, "gpu-w-1") {
		t.Fatal("forgotten Cluster still remembered")
	}
}

func TestReconcileDryRunForgetsMachinesNoLongerReleasable(t *testing.T) {
	node := newNode("gpu-w-1")
	f := newFixture(t, fixtureOptions{dryRun: true},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1", markedFor(capi.ActionRestart))}, node)

	assertPoll(t, f.reconcile(t))
	if !f.r.releasableBefore(clusterKey, "gpu-w-1") {
		t.Fatal("Machine not remembered")
	}

	// The signal returns and holds the Machine; a later clear is news again.
	node.Status.Conditions = []corev1.NodeCondition{raised("GpuXidError", restartMessage)}
	if err := f.workload.Status().Update(context.Background(), node); err != nil {
		t.Fatalf("update node: %v", err)
	}
	assertPoll(t, f.reconcile(t))
	if f.r.releasableBefore(clusterKey, "gpu-w-1") {
		t.Fatal("held Machine still remembered as releasable")
	}
}

func TestReconcileRetriesAFailedRelease(t *testing.T) {
	failing := true
	f := newFixture(t, fixtureOptions{hubFuncs: interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if failing {
				return errors.New("webhook denied the patch")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1", markedFor(capi.ActionRestart))},
		newNode("gpu-w-1"))

	assertPoll(t, f.reconcile(t))
	if !capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("Machine released despite the failed patch")
	}
	assertEvents(t, f.events())

	failing = false
	assertPoll(t, f.reconcile(t))
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("Machine not released on the next poll")
	}
	assertEvents(t, f.events(), "Normal RemediationReleased")
}

// pendingRequest builds an ExternalRemediationRequest the way NVSentinel's
// janitor leaves it once it has released the node.
func pendingRequest(name, node, check, action string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"healthEvent": map[string]any{
			"nodeName": node, "checkName": check, "recommendedAction": action, "errorCode": []any{"79"},
		}},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": extrr.ConditionOwnershipReleased, "status": "True"},
			map[string]any{"type": extrr.ConditionComplete, "status": "Unknown"},
		}},
	}}
	obj.SetGroupVersionKind(extrr.GroupVersionKind)
	obj.SetName(name)

	return obj
}

func TestReconcileReadsRequestsWhereTheyAreServed(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		workloadMapper: newMapper(extrr.GroupVersionKind, janitorGroupVersionKind),
		workloadObjs:   []client.Object{pendingRequest("extrr-1", "gpu-w-2", "SysLogsNICDriverError", "REPLACE_VM")},
	}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1"), newMachine("gpu-w-2", "gpu-w-2")},
		// The janitor remediates this condition itself; it is not ours.
		newNode("gpu-w-1", raised("GpuXidError", replaceMessage)),
		newNode("gpu-w-2"))

	assertPoll(t, f.reconcile(t))
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("a node condition was acted on where requests are served")
	}
	m := f.machine(t, "gpu-w-2")
	if !capi.IsMarkedForRemediation(m) {
		t.Fatal("the request's Machine was not marked")
	}
	wantReason := "ExternalRemediationRequest SysLogsNICDriverError 79 (REPLACE_VM): REPLACE_VM maps to Replace; " +
		"MachineHealthCheck gpu-workers remediates by replacement"
	if got := m.Annotations[capi.RemediationReasonAnnotation]; got != wantReason {
		t.Fatalf("reason annotation = %q, want %q", got, wantReason)
	}
	assertEvents(t, f.events(), "Warning MarkedForRemediation Marked for remediation: "+wantReason)
	assertEvents(t, f.sourceEvents, "Normal SignalSourceSelected ExternalRemediationRequest: reading NVSentinel's ExternalRemediationRequests")
}

func TestReconcileOnlyReportsWhereTheJanitorRemediates(t *testing.T) {
	f := newFixture(t, fixtureOptions{workloadMapper: newMapper(janitorGroupVersionKind)},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")},
		newNode("gpu-w-1", raised("GpuXidError", replaceMessage)))

	assertPoll(t, f.reconcile(t))
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("Machine marked where NVSentinel's janitor remediates")
	}
	assertEvents(t, f.events(), "Warning NodeHealthReported GpuXidError DCGM_FR_XID_ERROR gpu GPU-1234 (REPLACE_VM) on node gpu-w-1: "+
		"REPLACE_VM maps to Replace; reported only since NVSentinel's janitor remediates this cluster itself")
	assertEvents(t, f.sourceEvents, "Normal SignalSourceSelected ReportOnly")
	if obs, _ := f.observed("gpu-w-1/GpuXidError"); obs.Decision != decision.Report {
		t.Fatalf("observation = %+v, want a Report", obs)
	}
}

func TestReconcileRecordsModeChangesOnce(t *testing.T) {
	mapper := newMapper()
	f := newFixture(t, fixtureOptions{workloadMapper: mapper},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")}, newNode("gpu-w-1"))

	for range 2 {
		assertPoll(t, f.reconcile(t))
		f.events()
	}
	assertEvents(t, f.sourceEvents, "Normal SignalSourceSelected NodeCondition")

	// NVSentinel is upgraded with its janitor: the next poll notices.
	mapper.Add(extrr.GroupVersionKind, meta.RESTScopeRoot)
	f.sourceEvents = nil
	for range 2 {
		assertPoll(t, f.reconcile(t))
		f.events()
	}
	assertEvents(t, f.sourceEvents, "Normal SignalSourceSelected ExternalRemediationRequest")

	// Forgetting the Cluster forgets its mode: selecting it again is news.
	f.r.forget(clusterKey)
	f.sourceEvents = nil
	assertPoll(t, f.reconcile(t))
	f.events()
	assertEvents(t, f.sourceEvents, "Normal SignalSourceSelected ExternalRemediationRequest")
}

func TestReconcileDryRunRecordsNoModeEvent(t *testing.T) {
	f := newFixture(t, fixtureOptions{dryRun: true},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")}, newNode("gpu-w-1"))

	assertPoll(t, f.reconcile(t))
	f.events()
	assertEvents(t, f.sourceEvents)
}

func TestReconcileTreatsANewRequestAsNews(t *testing.T) {
	first := pendingRequest("extrr-1", "gpu-w-1", "SysLogsNICDriverError", "REPLACE_VM")
	f := newFixture(t, fixtureOptions{
		uncovered:      true,
		workloadMapper: requestMode(),
		workloadObjs:   []client.Object{first},
	}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")}, newNode("gpu-w-1"))

	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events(), "Warning ReplaceUnavailable", "Normal ExternalRemediationRequestAnswered")
	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events())

	// NVSentinel deletes the answered request once its time to live is
	// up, which releases its hold on the node, and raises the same fault
	// again: the same node and check, but a new request and a new fault.
	if err := f.workload.Delete(context.Background(), first); err != nil {
		t.Fatalf("delete request: %v", err)
	}
	if err := f.workload.Create(context.Background(), pendingRequest("extrr-2", "gpu-w-1", "SysLogsNICDriverError", "REPLACE_VM")); err != nil {
		t.Fatalf("create request: %v", err)
	}
	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events(), "Warning ReplaceUnavailable", "Normal ExternalRemediationRequestAnswered")
	if obs, _ := f.observed("gpu-w-1/SysLogsNICDriverError"); obs.Request != "extrr-2" {
		t.Fatalf("observation = %+v, want the new request", obs)
	}
}

// booted gives the node a boot ID and makes it Ready.
func booted(bootID string) func(*corev1.Node) {
	return func(n *corev1.Node) {
		n.Status.NodeInfo.BootID = bootID
		n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue})
	}
}

func bootedNode(name, bootID string) *corev1.Node {
	n := newNode(name)
	booted(bootID)(n)
	return n
}

// requestAnswer returns the status and reason of a request's answer.
func (f *fixture) requestAnswer(t *testing.T, name string) (string, string) {
	t.Helper()

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(extrr.GroupVersionKind)
	if err := f.workload.Get(context.Background(), client.ObjectKey{Name: name}, obj); err != nil {
		t.Fatalf("get request %s: %v", name, err)
	}
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conditions {
		if cond := c.(map[string]any); cond["type"] == extrr.ConditionComplete {
			status, _ := cond["status"].(string)
			reason, _ := cond["reason"].(string)
			return status, reason
		}
	}

	return "", ""
}

func requestMode() meta.RESTMapper {
	return newMapper(extrr.GroupVersionKind, janitorGroupVersionKind)
}

func TestReconcileRestartsThroughARequestAndAnswersOnceBack(t *testing.T) {
	node := bootedNode("gpu-w-1", "boot-1")
	f := newFixture(t, fixtureOptions{
		healthChecks:   []*clusterv1.MachineHealthCheck{newHealthCheck("reboot", withTemplate)},
		workloadMapper: requestMode(),
		workloadObjs:   []client.Object{pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "RESTART_BM")},
	}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")}, node)

	assertPoll(t, f.reconcile(t))
	m := f.machine(t, "gpu-w-1")
	if action, _ := capi.MarkedAction(m); action != capi.ActionRestart {
		t.Fatalf("MarkedAction = %q, want the Machine marked for a restart", action)
	}
	if got := m.Annotations[capi.RemediationBootIDAnnotation]; got != "boot-1" {
		t.Fatalf("recorded boot ID = %q, want the node's", got)
	}
	assertEvents(t, f.events(), "Warning MarkedForRemediation")
	if status, _ := f.requestAnswer(t, "extrr-1"); status != "Unknown" {
		t.Fatalf("request answered %q while the restart is pending", status)
	}

	// Restarting: a new boot, not Ready yet. Nothing to say.
	node.Status.NodeInfo.BootID = "boot-2"
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	if err := f.workload.Status().Update(context.Background(), node); err != nil {
		t.Fatalf("update node: %v", err)
	}
	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events())
	if status, _ := f.requestAnswer(t, "extrr-1"); status != "Unknown" {
		t.Fatalf("request answered %q before the node is back", status)
	}

	// Back and Ready: the request is answered, then the Machine released.
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if err := f.workload.Status().Update(context.Background(), node); err != nil {
		t.Fatalf("update node: %v", err)
	}
	assertPoll(t, f.reconcile(t))
	if status, reason := f.requestAnswer(t, "extrr-1"); status != "True" || reason != AnswerRestarted {
		t.Fatalf("answer = %s %s, want True %s", status, reason, AnswerRestarted)
	}
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("Machine still marked after its restart was answered")
	}
	assertEvents(t, f.events(),
		"Normal ExternalRemediationRequestAnswered Answered ExternalRemediationRequest extrr-1: True, Restarted: node gpu-w-1 restarted and is Ready",
		"Normal RemediationReleased Released from remediation: node gpu-w-1 restarted and is Ready")

	// The answered request is no longer pending: nothing more happens.
	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events())
}

func TestReconcileDeclinesRequestsItWillNotActOn(t *testing.T) {
	tests := []struct {
		name    string
		opts    fixtureOptions
		machine *clusterv1.Machine
		action  string
		reason  string
		event   string
	}{
		{"reported action", fixtureOptions{}, newMachine("gpu-w-1", "gpu-w-1"), "CONTACT_SUPPORT",
			AnswerNotRemediated, "Warning NodeHealthReported"},
		{"no Machine", fixtureOptions{}, nil, "REPLACE_VM", AnswerMachineNotFound, "Warning MachineNotFound"},
		{"control plane", fixtureOptions{}, newMachine("gpu-w-1", "gpu-w-1", controlPlane), "REPLACE_VM",
			string(capi.SkipControlPlane), "Warning RemediationSkipped"},
		{"opted out", fixtureOptions{}, newMachine("gpu-w-1", "gpu-w-1", optedOut), "REPLACE_VM",
			string(capi.SkipOptedOut), "Warning RemediationSkipped"},
		{"replacement without a health check", fixtureOptions{uncovered: true}, newMachine("gpu-w-1", "gpu-w-1"), "REPLACE_VM",
			AnswerRemediationUnavailable, "Warning ReplaceUnavailable"},
		{"restart without a template", fixtureOptions{}, newMachine("gpu-w-1", "gpu-w-1"), "RESTART_BM",
			AnswerRemediationUnavailable, "Warning RestartUnavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := tt.opts
			opts.workloadMapper = requestMode()
			opts.workloadObjs = []client.Object{pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", tt.action)}
			hubObjs := []client.Object{newCluster()}
			if tt.machine != nil {
				hubObjs = append(hubObjs, tt.machine)
			}
			f := newFixture(t, opts, hubObjs, bootedNode("gpu-w-1", "boot-1"))

			assertPoll(t, f.reconcile(t))
			if status, reason := f.requestAnswer(t, "extrr-1"); status != "False" || reason != tt.reason {
				t.Fatalf("answer = %s %s, want False %s", status, reason, tt.reason)
			}
			if tt.machine != nil && capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
				t.Fatal("a declined request marked the Machine")
			}
			assertEvents(t, f.events(), tt.event, "Normal ExternalRemediationRequestAnswered Answered ExternalRemediationRequest extrr-1: False, "+tt.reason)

			assertPoll(t, f.reconcile(t))
			assertEvents(t, f.events())
		})
	}
}

func TestReconcileWaitsOnRequestsStillInProgress(t *testing.T) {
	paused := func(m *clusterv1.Machine) { m.Annotations = map[string]string{clusterv1.PausedAnnotation: ""} }
	tests := []struct {
		name    string
		machine *clusterv1.Machine
		nodes   []*corev1.Node
		action  string
	}{
		// A paused Machine is acted on once it is unpaused.
		{"paused Machine", newMachine("gpu-w-1", "gpu-w-1", paused), []*corev1.Node{bootedNode("gpu-w-1", "boot-1")}, "REPLACE_VM"},
		// Cluster API replaces the Machine and deletes the node, and the
		// request with it.
		{"replacement under way", newMachine("gpu-w-1", "gpu-w-1", markedFor(capi.ActionRemediate)),
			[]*corev1.Node{bootedNode("gpu-w-1", "boot-1")}, "REPLACE_VM"},
		// The restart has not happened yet.
		{"restart under way", newMachine("gpu-w-1", "gpu-w-1", markedFor(capi.ActionRestart), func(m *clusterv1.Machine) {
			m.Annotations[capi.RemediationBootIDAnnotation] = "boot-1"
		}), []*corev1.Node{bootedNode("gpu-w-1", "boot-1")}, "RESTART_BM"},
		// Replaced already: neither Machine nor node is left.
		{"node gone", nil, nil, "REPLACE_VM"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hubObjs := []client.Object{newCluster()}
			if tt.machine != nil {
				hubObjs = append(hubObjs, tt.machine)
			}
			f := newFixture(t, fixtureOptions{
				healthChecks:   []*clusterv1.MachineHealthCheck{newHealthCheck("reboot", withTemplate)},
				workloadMapper: requestMode(),
				workloadObjs:   []client.Object{pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", tt.action)},
			}, hubObjs, tt.nodes...)

			for range 2 {
				assertPoll(t, f.reconcile(t))
				if status, _ := f.requestAnswer(t, "extrr-1"); status != "Unknown" {
					t.Fatalf("request answered %q while remediation is in progress", status)
				}
			}
			for _, e := range f.events() {
				if strings.Contains(e, EventRequestAnswered) || strings.Contains(e, EventMachineNotFound) {
					t.Fatalf("unexpected event %q", e)
				}
			}
		})
	}
}

func TestReconcileDryRunAnswersNothing(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		dryRun:         true,
		workloadMapper: requestMode(),
		workloadObjs:   []client.Object{pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "CONTACT_SUPPORT")},
	}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")}, bootedNode("gpu-w-1", "boot-1"))

	assertPoll(t, f.reconcile(t))
	if status, _ := f.requestAnswer(t, "extrr-1"); status != "Unknown" {
		t.Fatalf("dry run answered %q", status)
	}
	assertEvents(t, f.events())
	if obs, _ := f.observed("gpu-w-1/SysLogsXIDError"); obs.Answer.Complete || obs.Answer.Reason != AnswerNotRemediated {
		t.Fatalf("observation = %+v, want the answer it would give", obs)
	}
}

func TestReconcileRetriesAFailedAnswer(t *testing.T) {
	failing := true
	f := newFixture(t, fixtureOptions{
		workloadMapper: requestMode(),
		workloadObjs:   []client.Object{pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "CONTACT_SUPPORT")},
		workloadFuncs: interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if failing {
					return errors.New("apiserver unavailable")
				}
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		},
	}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")}, bootedNode("gpu-w-1", "boot-1"))

	assertPoll(t, f.reconcile(t))
	if status, _ := f.requestAnswer(t, "extrr-1"); status != "Unknown" {
		t.Fatalf("answer written despite the failure: %q", status)
	}
	assertEvents(t, f.events(), "Warning NodeHealthReported")

	failing = false
	assertPoll(t, f.reconcile(t))
	if status, _ := f.requestAnswer(t, "extrr-1"); status != "False" {
		t.Fatalf("answer = %q, want the retry to answer False", status)
	}
	assertEvents(t, f.events(), "Normal ExternalRemediationRequestAnswered")
}

func TestReconcileRetriesWhenNodesCannotBeRead(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		workloadMapper: requestMode(),
		workloadObjs:   []client.Object{pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "CONTACT_SUPPORT")},
		workloadFuncs: interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.NodeList); ok {
					return errors.New("apiserver unavailable")
				}
				return c.List(ctx, list, opts...)
			},
		},
	}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")}, bootedNode("gpu-w-1", "boot-1"))

	assertPoll(t, f.reconcile(t))
	if status, _ := f.requestAnswer(t, "extrr-1"); status != "Unknown" {
		t.Fatalf("request answered %q without its node", status)
	}
	assertEvents(t, f.events())
}

// requestReads returns interceptor functions that answer the n-th and
// later reads of a request with the given error, as when the request goes
// away in the middle of a poll.
func requestReads(fromRead int, err error) interceptor.Funcs {
	reads := 0
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*unstructured.Unstructured); ok {
				reads++
				if reads >= fromRead {
					return err
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
}

func TestReconcileIgnoresRequestsNoLongerPending(t *testing.T) {
	// The cached list still shows the request, but the API server no
	// longer has it: nothing is done about it.
	gone := apierrors.NewNotFound(schema.GroupResource{Group: extrr.GroupVersionKind.Group}, "extrr-1")
	f := newFixture(t, fixtureOptions{
		workloadMapper: requestMode(),
		workloadObjs:   []client.Object{pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "REPLACE_VM")},
		workloadFuncs:  requestReads(1, gone),
	}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")}, bootedNode("gpu-w-1", "boot-1"))

	assertPoll(t, f.reconcile(t))
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("Machine marked for a request that is no longer pending")
	}
	assertEvents(t, f.events())
	if _, ok := f.observed("gpu-w-1/SysLogsXIDError"); ok {
		t.Fatal("a request no longer pending was remembered")
	}
}

func TestReconcileKeepsWhatItKnewWhenARequestCannotBeRead(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		workloadMapper: requestMode(),
		workloadObjs:   []client.Object{pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "CONTACT_SUPPORT")},
		// In dry run a poll reads the request once, to check it is pending.
		workloadFuncs: requestReads(2, errors.New("apiserver unavailable")),
		dryRun:        true,
	}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")}, bootedNode("gpu-w-1", "boot-1"))

	assertPoll(t, f.reconcile(t))
	before, ok := f.observed("gpu-w-1/SysLogsXIDError")
	if !ok {
		t.Fatal("signal not remembered")
	}

	// The next read fails: the signal is neither handled nor forgotten.
	assertPoll(t, f.reconcile(t))
	if after, ok := f.observed("gpu-w-1/SysLogsXIDError"); !ok || after != before {
		t.Fatalf("observation = %+v, %v; want it kept as %+v", after, ok, before)
	}
}

func TestReconcileReleasesNothingWhenARequestCannotBeRead(t *testing.T) {
	// First poll after a restart of the operator: nothing is remembered,
	// and the request holding the Machine cannot be read. The restart under
	// way must not be withdrawn.
	f := newFixture(t, fixtureOptions{
		healthChecks:   []*clusterv1.MachineHealthCheck{newHealthCheck("reboot", withTemplate)},
		workloadMapper: requestMode(),
		workloadObjs:   []client.Object{pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "RESTART_BM")},
		workloadFuncs:  requestReads(1, errors.New("apiserver unavailable")),
	}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1", markedFor(capi.ActionRestart), func(m *clusterv1.Machine) {
		m.Annotations[capi.RemediationBootIDAnnotation] = "boot-1"
	})}, bootedNode("gpu-w-1", "boot-1"))

	assertPoll(t, f.reconcile(t))
	if action, _ := capi.MarkedAction(f.machine(t, "gpu-w-1")); action != capi.ActionRestart {
		t.Fatalf("MarkedAction = %q, want the restart kept while its request cannot be read", action)
	}
	assertEvents(t, f.events())
}

func TestReconcileSaysNothingWhenTheRequestIsGoneBeforeTheAnswer(t *testing.T) {
	// The request is pending when the poll checks it and gone when the
	// answer reads it again: there is nobody left to answer.
	gone := apierrors.NewNotFound(schema.GroupResource{Group: extrr.GroupVersionKind.Group}, "extrr-1")
	f := newFixture(t, fixtureOptions{
		workloadMapper: requestMode(),
		workloadObjs:   []client.Object{pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "CONTACT_SUPPORT")},
		workloadFuncs:  requestReads(2, gone),
	}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")}, bootedNode("gpu-w-1", "boot-1"))

	assertPoll(t, f.reconcile(t))
	assertEvents(t, f.events(), "Warning NodeHealthReported")
}

func TestReconcileReleasesARestartWhoseReleaseFailedAfterTheAnswer(t *testing.T) {
	failing := true
	f := newFixture(t, fixtureOptions{
		healthChecks:   []*clusterv1.MachineHealthCheck{newHealthCheck("reboot", withTemplate)},
		workloadMapper: requestMode(),
		workloadObjs:   []client.Object{pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "RESTART_BM")},
		hubFuncs: interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if failing {
					return errors.New("webhook denied the patch")
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		},
	}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1", markedFor(capi.ActionRestart), func(m *clusterv1.Machine) {
		m.Annotations[capi.RemediationBootIDAnnotation] = "boot-1"
	})}, bootedNode("gpu-w-1", "boot-2"))

	assertPoll(t, f.reconcile(t))
	if status, _ := f.requestAnswer(t, "extrr-1"); status != "True" {
		t.Fatalf("answer = %q, want True", status)
	}
	if !capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("Machine released despite the failed patch")
	}

	// The answered request no longer holds the Machine.
	failing = false
	assertPoll(t, f.reconcile(t))
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("Machine not released on the next poll")
	}
}

func TestReconcileStartsOverWhenTheModeChanges(t *testing.T) {
	mapper := newMapper()
	f := newFixture(t, fixtureOptions{workloadMapper: mapper},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")},
		newNode("gpu-w-1", raised("GpuThermal", reportMessage)))

	assertPoll(t, f.reconcile(t))
	if _, ok := f.observed("gpu-w-1/GpuThermal"); !ok {
		t.Fatal("signal not remembered")
	}

	// The janitor arrives with requests: the condition is no longer this
	// operator's, and what was concluded about it is dropped rather than
	// reported as cleared. noteMode is called directly because the end of
	// a reconcile replaces what was concluded anyway, hiding the drop.
	f.r.noteMode(ctrl.Log, newCluster(), ModeExternalRemediationRequest)
	if _, ok := f.observed("gpu-w-1/GpuThermal"); ok {
		t.Fatal("observations of the old mode kept")
	}
}

func TestReconcileRetriesWhenTheSourceCannotBeSelected(t *testing.T) {
	boom := errors.New("discovery unavailable")
	f := newFixture(t, fixtureOptions{workloadMapper: failingMapper{RESTMapper: newMapper(), group: extrr.GroupVersionKind.Group, err: boom}},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")},
		newNode("gpu-w-1", raised("GpuXidError", replaceMessage)))

	assertPoll(t, f.reconcile(t))
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("Machine marked although the source could not be selected")
	}
	assertEvents(t, f.events())
	assertEvents(t, f.sourceEvents)
}

func TestReconcileWaitsForPausedHealthChecks(t *testing.T) {
	pausedCheck := func(mhc *clusterv1.MachineHealthCheck) {
		mhc.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
	}
	tests := []struct {
		name     string
		checks   []*clusterv1.MachineHealthCheck
		action   string
		fallback RestartFallback
		declined bool
	}{
		// Unpausing the check makes the decision possible: wait for it.
		{"replacement", []*clusterv1.MachineHealthCheck{newHealthCheck("replace", pausedCheck)}, "REPLACE_VM", "", false},
		{"restart", []*clusterv1.MachineHealthCheck{newHealthCheck("reboot", withTemplate, pausedCheck)}, "RESTART_BM", "", false},
		{"restart falling back to a replacement", []*clusterv1.MachineHealthCheck{newHealthCheck("replace", pausedCheck)},
			"RESTART_BM", RestartFallbackReplace, false},
		// Unpausing it would not help: a check without a template replaces
		// the Machine, which a restart must not do.
		{"restart only a replacement could follow", []*clusterv1.MachineHealthCheck{newHealthCheck("replace", pausedCheck)},
			"RESTART_BM", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, fixtureOptions{
				healthChecks:    tt.checks,
				restartFallback: tt.fallback,
				workloadMapper:  requestMode(),
				workloadObjs:    []client.Object{pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", tt.action)},
			}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")}, bootedNode("gpu-w-1", "boot-1"))

			assertPoll(t, f.reconcile(t))
			status, _ := f.requestAnswer(t, "extrr-1")
			if declined := status == "False"; declined != tt.declined {
				t.Fatalf("answer = %q, want declined: %v", status, tt.declined)
			}
			if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
				t.Fatal("Machine marked while its only check is paused")
			}
		})
	}
}

func TestReconcileAnswersAFailedRestartAnswerAgainWithoutRestarting(t *testing.T) {
	failing := true
	f := newFixture(t, fixtureOptions{
		healthChecks:   []*clusterv1.MachineHealthCheck{newHealthCheck("reboot", withTemplate)},
		workloadMapper: requestMode(),
		workloadObjs:   []client.Object{pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "RESTART_BM")},
		workloadFuncs: interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if failing {
					return errors.New("apiserver unavailable")
				}
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		},
	}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1", markedFor(capi.ActionRestart), func(m *clusterv1.Machine) {
		m.Annotations[capi.RemediationBootIDAnnotation] = "boot-1"
	})}, bootedNode("gpu-w-1", "boot-2"))

	// The restart is over but the answer fails: the Machine stays marked
	// as it was, and is not marked again.
	assertPoll(t, f.reconcile(t))
	m := f.machine(t, "gpu-w-1")
	if action, _ := capi.MarkedAction(m); action != capi.ActionRestart || m.Annotations[capi.RemediationBootIDAnnotation] != "boot-1" {
		t.Fatalf("mark changed after a failed answer: %v", m.Annotations)
	}
	assertEvents(t, f.events())

	failing = false
	assertPoll(t, f.reconcile(t))
	if status, reason := f.requestAnswer(t, "extrr-1"); status != "True" || reason != AnswerRestarted {
		t.Fatalf("answer = %s %s, want True %s", status, reason, AnswerRestarted)
	}
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("Machine still marked after the answer")
	}
	for _, e := range f.events() {
		if strings.Contains(e, capi.EventMarkedForRemediation) {
			t.Fatalf("Machine marked again: %q", e)
		}
	}
}

func TestReconcileKeepsARestartWhoseConditionPersistsAfterTheBoot(t *testing.T) {
	// Node conditions are lowered by NVSentinel once the fault is gone. One
	// that is still raised after the node restarted means the restart did
	// not help: the Machine stays marked, and a restart is not asked for
	// over and over.
	node := bootedNode("gpu-w-1", "boot-2")
	node.Status.Conditions = append(node.Status.Conditions, raised("GpuXidError", restartMessage))
	f := newFixture(t, fixtureOptions{healthChecks: []*clusterv1.MachineHealthCheck{newHealthCheck("reboot", withTemplate)}},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1", markedFor(capi.ActionRestart), func(m *clusterv1.Machine) {
			m.Annotations[capi.RemediationBootIDAnnotation] = "boot-1"
		})}, node)

	for range 2 {
		assertPoll(t, f.reconcile(t))
		if action, _ := capi.MarkedAction(f.machine(t, "gpu-w-1")); action != capi.ActionRestart {
			t.Fatalf("MarkedAction = %q, want the restart mark kept", action)
		}
	}
	for _, e := range f.events() {
		if strings.Contains(e, capi.EventRemediationReleased) || strings.Contains(e, capi.EventMarkedForRemediation) {
			t.Fatalf("unexpected event %q", e)
		}
	}
}

// uncachedFailingCache is a ClusterCache whose uncached client cannot be
// had.
type uncachedFailingCache struct {
	clustercache.ClusterCache
	err error
}

func (c uncachedFailingCache) GetUncachedClient(context.Context, client.ObjectKey) (client.Client, error) {
	return nil, c.err
}

func TestReconcileRetriesWhenTheWorkloadClusterCannotBeReachedUncached(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		workloadMapper: requestMode(),
		workloadObjs:   []client.Object{pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "CONTACT_SUPPORT")},
	}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")}, bootedNode("gpu-w-1", "boot-1"))
	f.r.ClusterCache = uncachedFailingCache{ClusterCache: f.r.ClusterCache, err: errors.New("unreachable")}

	assertPoll(t, f.reconcile(t))
	if status, _ := f.requestAnswer(t, "extrr-1"); status != "Unknown" {
		t.Fatalf("request answered %q", status)
	}
	assertEvents(t, f.events())
}

// failingCache is a ClusterCache whose reader fails for a reason other than
// a missing connection.
type failingCache struct {
	clustercache.ClusterCache
	err error
}

func (c failingCache) GetClient(context.Context, client.ObjectKey) (client.Client, error) {
	return nil, c.err
}

func TestReconcileReturnsErrorsWorthRetrying(t *testing.T) {
	boom := errors.New("boom")
	hubObjs := []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1")}
	node := newNode("gpu-w-1", raised("GpuXidError", replaceMessage))

	tests := []struct {
		name  string
		setup func(*fixture)
		opts  fixtureOptions
	}{
		{"reading the Cluster fails", func(*fixture) {}, fixtureOptions{hubFuncs: interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return boom
			},
		}}},
		{"listing Machines fails", func(*fixture) {}, fixtureOptions{hubFuncs: interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return boom
			},
		}}},
		{"listing MachineHealthChecks fails", func(*fixture) {}, fixtureOptions{hubFuncs: interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*clusterv1.MachineHealthCheckList); ok {
					return boom
				}
				return c.List(ctx, list, opts...)
			},
		}}},
		{"the cluster cache fails", func(f *fixture) {
			f.r.ClusterCache = failingCache{ClusterCache: f.r.ClusterCache, err: boom}
		}, fixtureOptions{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.opts, hubObjs, node)
			tt.setup(f)

			_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: clusterKey})
			if !errors.Is(err, boom) {
				t.Fatalf("Reconcile error = %v, want it to wrap %v", err, boom)
			}
			assertEvents(t, f.events())
		})
	}
}

func TestPollIntervalDefaults(t *testing.T) {
	if got := (&ClusterReconciler{}).pollInterval(); got != DefaultPollInterval {
		t.Fatalf("pollInterval() = %v, want %v", got, DefaultPollInterval)
	}
	if got := (&ClusterReconciler{PollInterval: time.Second}).pollInterval(); got != time.Second {
		t.Fatalf("pollInterval() = %v, want 1s", got)
	}
}

func TestClusterToRequest(t *testing.T) {
	got := clusterToRequest(context.Background(), newCluster())
	if len(got) != 1 || got[0].NamespacedName != clusterKey {
		t.Fatalf("clusterToRequest() = %v, want %v", got, clusterKey)
	}
}

func TestDescribe(t *testing.T) {
	tests := []struct {
		name string
		sig  signal.Signal
		want string
	}{
		{"check only", signal.Signal{Check: "GpuXidError"}, "GpuXidError"},
		{"everything", signal.Signal{Check: "GpuXidError", ErrorCodes: []string{"A", "B"}, GpuUUIDs: []string{"GPU-1"},
			Actions: []string{"RESTART_BM", "REPLACE_VM"}, Truncated: true},
			"GpuXidError A,B gpu GPU-1 (RESTART_BM,REPLACE_VM) [truncated]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := describe(tt.sig); got != tt.want {
				t.Fatalf("describe() = %q, want %q", got, tt.want)
			}
		})
	}
}
