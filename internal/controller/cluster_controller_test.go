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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
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
}

type fixtureOptions struct {
	dryRun        bool
	disconnected  bool
	selector      labels.Selector
	hubFuncs      interceptor.Funcs
	workloadFuncs interceptor.Funcs
}

func newFixture(t *testing.T, opts fixtureOptions, hubObjs []client.Object, nodes ...*corev1.Node) *fixture {
	t.Helper()

	scheme := newScheme(t)
	hub := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hubObjs...).WithInterceptorFuncs(opts.hubFuncs).Build()

	nodeObjs := make([]client.Object, 0, len(nodes))
	for _, n := range nodes {
		nodeObjs = append(nodeObjs, n)
	}
	workload := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodeObjs...).WithInterceptorFuncs(opts.workloadFuncs).Build()

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

func (f *fixture) events() []string {
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
	wantReason := "NodeCondition GpuXidError DCGM_FR_XID_ERROR gpu GPU-1234 (REPLACE_VM): REPLACE_VM maps to Replace"
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
	assertEvents(t, f.events(), "Warning RestartUnavailable GpuXidError DCGM_FR_XID_ERROR (RESTART_BM) calls for a restart of node gpu-w-1, which is not available yet; reported only")
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

// failingCache is a ClusterCache whose reader fails for a reason other than
// a missing connection.
type failingCache struct {
	clustercache.ClusterCache
	err error
}

func (c failingCache) GetReader(context.Context, client.ObjectKey) (client.Reader, error) {
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
