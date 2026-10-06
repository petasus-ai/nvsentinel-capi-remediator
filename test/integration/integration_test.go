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

// Package integration runs the manager against two real API servers, one
// standing in for the management cluster and one for a workload cluster,
// to cover what the fake clients of the unit tests cannot: reaching the
// workload cluster through its kubeconfig Secret, choosing the signal
// source from what that cluster's discovery serves, and answering requests
// through their status subresource.
package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/capi"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/controller"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/decision"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal/extrr"
)

const (
	namespace   = "tenant-a"
	clusterName = "gpu"

	// poolLabel splits the Machines between the two health checks: one
	// that replaces and one that remediates through a template.
	poolLabel   = "pool"
	poolReplace = "replace"
	poolRestart = "restart"

	// pollInterval is short so that a test waits for a few polls at most.
	pollInterval = 300 * time.Millisecond
	// wait bounds every expectation. It is generous because the first poll
	// only happens once the cluster cache has connected.
	wait = 60 * time.Second
)

var (
	hub      client.Client
	workload client.Client
	// workloadEnv is kept for the test that installs the request CRD
	// while the manager is running.
	workloadEnv *envtest.Environment
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run starts both API servers and the manager, runs the tests and stops
// everything again. It is separate from TestMain so that the deferred
// cleanup runs before os.Exit.
func run(m *testing.M) int {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	crds, err := clusterAPICRDs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "locating the Cluster API CRDs:", err)
		return 1
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := clusterv1.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	hubEnv := &envtest.Environment{CRDDirectoryPaths: []string{crds}, ErrorIfCRDPathMissing: true}
	hubCfg, err := hubEnv.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting the management API server:", err)
		return 1
	}
	defer stop("management", hubEnv)

	workloadEnv = &envtest.Environment{}
	workloadCfg, err := workloadEnv.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting the workload API server:", err)
		return 1
	}
	defer stop("workload", workloadEnv)

	if hub, err = client.New(hubCfg, client.Options{Scheme: scheme}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if workload, err = client.New(workloadCfg, client.Options{Scheme: scheme}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	ctx, cancel := context.WithCancel(ctrl.SetupSignalHandler())
	clusterCache, err := startManager(ctx, hubCfg, scheme)
	if err != nil {
		cancel()
		fmt.Fprintln(os.Stderr, "starting the manager:", err)
		return 1
	}
	// Deferred calls run last in first out: the manager stops, then the
	// cluster cache lets go of its watches on the workload cluster, and only
	// then do the API servers stop. The cluster cache does not stop with the
	// manager's context, and an API server waits for open watches.
	defer func() {
		cancel()
		if c, ok := clusterCache.(interface{ Shutdown() }); ok {
			c.Shutdown()
		}
	}()

	return m.Run()
}

// stop stops an API server and says so when that fails, since a server left
// running holds its port and data directory.
func stop(name string, env *envtest.Environment) {
	if err := env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stopping the %s API server: %v\n", name, err)
	}
}

// clusterAPICRDs returns the directory of the Cluster API core CRDs in the
// module cache, so that the tests run against the CRDs of the version the
// module requires. The types come from the separate cluster-api/api module,
// which go.mod keeps at the same version.
func clusterAPICRDs() (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/cluster-api").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", fmt.Errorf("%w: %s", err, exit.Stderr)
		}

		return "", err
	}

	return filepath.Join(strings.TrimSpace(string(out)), "core", "config", "crd", "bases"), nil
}

// startManager runs the controller the way cmd/manager wires it, acting
// for real and polling fast, and returns the cluster cache for the caller
// to shut down.
func startManager(ctx context.Context, cfg *rest.Config, scheme *runtime.Scheme) (clustercache.ClusterCache, error) {
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		return nil, err
	}

	clusterCache, err := clustercache.SetupWithManager(ctx, mgr, clustercache.Options{
		SecretClient: mgr.GetAPIReader(),
		Cache: clustercache.CacheOptions{
			DefaultTransform: cache.TransformStripManagedFields(),
		},
		Client: clustercache.ClientOptions{
			UserAgent: controller.ControllerName,
			Cache: clustercache.ClientCacheOptions{
				DisableFor: []client.Object{&corev1.ConfigMap{}, &corev1.Secret{}},
			},
		},
	}, ctrlcontroller.Options{MaxConcurrentReconciles: 10})
	if err != nil {
		return nil, err
	}

	reconciler := &controller.ClusterReconciler{
		Client:       mgr.GetClient(),
		ClusterCache: clusterCache,
		Recorder:     mgr.GetEventRecorder(controller.ControllerName),
		Table:        decision.Default(),
		PollInterval: pollInterval,
	}
	if err := reconciler.SetupWithManager(ctx, mgr, ctrlcontroller.Options{MaxConcurrentReconciles: 1}); err != nil {
		return nil, err
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "manager exited:", err)
		}
	}()

	return clusterCache, nil
}

// TestRemediation walks one workload cluster through its life: read by
// node conditions first, then by requests once it serves them. The steps
// share the cluster and build on each other, so they run in order, the
// first failure ends the test, and the test cannot be repeated with -count
// in one process.
func TestRemediation(t *testing.T) {
	ctx := context.Background()
	createCluster(ctx, t)

	step := func(name string, f func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, f) {
			t.FailNow()
		}
	}

	step("a node condition is acted on while no request API is served", func(t *testing.T) {
		createMachine(ctx, t, "gpu-w-1", poolReplace)
		createNode(ctx, t, "gpu-w-1", "boot-1", corev1.NodeCondition{
			Type:    "SysLogsNICDriverError",
			Status:  corev1.ConditionTrue,
			Message: "ErrorCode:NIC_UNRECOVERABLE Recommended Action=REPLACE_VM;",
		})

		eventually(t, "the Machine to be marked for remediation", func() (bool, string) {
			m := getMachine(ctx, t, "gpu-w-1")
			return capi.IsMarkedForRemediation(m), fmt.Sprint(m.Annotations)
		})
		eventually(t, "an Event about marking the Machine", func() (bool, string) {
			return hasEvent(ctx, t, "Machine", "gpu-w-1", capi.EventMarkedForRemediation)
		})
	})

	step("requests become the source once the cluster serves them", func(t *testing.T) {
		if _, err := envtest.InstallCRDs(workloadEnv.Config, envtest.CRDInstallOptions{
			Paths:              []string{filepath.Join("testdata", "external_remediation.crd.yaml")},
			ErrorIfPathMissing: true,
		}); err != nil {
			t.Fatalf("install the request CRD: %v", err)
		}

		// Requests are only read in the mode the CRD brings, so one being
		// answered shows the switch. The table reports this one, which is
		// answered False.
		createMachine(ctx, t, "gpu-w-2", poolReplace)
		createNode(ctx, t, "gpu-w-2", "boot-1")
		createRequest(ctx, t, "extrr-report", "gpu-w-2", "GpuNvlinkWatch", "CONTACT_SUPPORT")

		eventually(t, "the request to be answered False", func() (bool, string) {
			status, reason := answer(ctx, t, "extrr-report")
			return status == "False" && reason == controller.AnswerNotRemediated, status + "/" + reason
		})
		if capi.IsMarkedForRemediation(getMachine(ctx, t, "gpu-w-2")) {
			t.Error("a reported request marked its Machine")
		}

		// From here on a node condition is NVSentinel's own to remediate.
		// This one is checked at the end, several polls later.
		createMachine(ctx, t, "gpu-w-5", poolReplace)
		createNode(ctx, t, "gpu-w-5", "boot-1", corev1.NodeCondition{
			Type:    "SysLogsNICDriverError",
			Status:  corev1.ConditionTrue,
			Message: "ErrorCode:NIC_UNRECOVERABLE Recommended Action=REPLACE_VM;",
		})
	})

	step("a replacement marks the Machine and leaves the request open", func(t *testing.T) {
		createMachine(ctx, t, "gpu-w-3", poolReplace)
		createNode(ctx, t, "gpu-w-3", "boot-1")
		createRequest(ctx, t, "extrr-replace", "gpu-w-3", "SysLogsNICDriverError", "REPLACE_VM")

		eventually(t, "the Machine to be marked for remediation", func() (bool, string) {
			m := getMachine(ctx, t, "gpu-w-3")
			return capi.IsMarkedForRemediation(m), fmt.Sprint(m.Annotations)
		})
		// The node goes with its Machine and takes the request with it, so
		// nothing is answered, on this poll or the following ones.
		consistently(t, "the replacement request to stay open", func() (bool, string) {
			status, reason := answer(ctx, t, "extrr-replace")
			return status == "Unknown", status + "/" + reason
		})
	})

	step("a restart is answered True once the node is back", func(t *testing.T) {
		createMachine(ctx, t, "gpu-w-4", poolRestart)
		createNode(ctx, t, "gpu-w-4", "boot-1")
		createRequest(ctx, t, "extrr-restart", "gpu-w-4", "SysLogsXIDError", "RESTART_BM")

		eventually(t, "the Machine to be marked for a restart", func() (bool, string) {
			m := getMachine(ctx, t, "gpu-w-4")
			return capi.IsMarkedForRemediation(m) && m.Annotations[capi.RemediationBootIDAnnotation] == "boot-1" &&
				m.Annotations[capi.RemediationRequestAnnotation] == "extrr-restart", fmt.Sprint(m.Annotations)
		})
		consistently(t, "the restart to stay open until the node restarts", func() (bool, string) {
			status, reason := answer(ctx, t, "extrr-restart")
			m := getMachine(ctx, t, "gpu-w-4")
			return status == "Unknown" && capi.IsMarkedForRemediation(m), status + "/" + reason + " " + fmt.Sprint(m.Annotations)
		})

		// The provider's remediation would restart the node now; a new boot
		// ID on a Ready node is how that shows.
		setBootID(ctx, t, "gpu-w-4", "boot-2")

		eventually(t, "the request to be answered True", func() (bool, string) {
			status, reason := answer(ctx, t, "extrr-restart")
			return status == "True" && reason == controller.AnswerRestarted, status + "/" + reason
		})
		eventually(t, "the Machine to be released", func() (bool, string) {
			m := getMachine(ctx, t, "gpu-w-4")
			return !capi.IsMarkedForRemediation(m), fmt.Sprint(m.Annotations)
		})
	})

	step("a request nothing can carry out is answered False", func(t *testing.T) {
		// No MachineHealthCheck selects this Machine, so marking it would
		// do nothing.
		createMachine(ctx, t, "gpu-w-6", "uncovered")
		createNode(ctx, t, "gpu-w-6", "boot-1")
		createRequest(ctx, t, "extrr-uncovered", "gpu-w-6", "SysLogsNICDriverError", "REPLACE_VM")
		// No Machine owns this node.
		createNode(ctx, t, "gpu-w-7", "boot-1")
		createRequest(ctx, t, "extrr-unowned", "gpu-w-7", "SysLogsNICDriverError", "REPLACE_VM")

		eventually(t, "the uncovered request to be answered False", func() (bool, string) {
			status, reason := answer(ctx, t, "extrr-uncovered")
			return status == "False" && reason == controller.AnswerRemediationUnavailable, status + "/" + reason
		})
		eventually(t, "the unowned request to be answered False", func() (bool, string) {
			status, reason := answer(ctx, t, "extrr-unowned")
			return status == "False" && reason == controller.AnswerMachineNotFound, status + "/" + reason
		})
		if capi.IsMarkedForRemediation(getMachine(ctx, t, "gpu-w-6")) {
			t.Error("an uncovered Machine was marked")
		}
	})

	step("a restart without a template follows the restart fallback", func(t *testing.T) {
		// The replacing check has no template, so by default the restart
		// is declined.
		createMachine(ctx, t, "gpu-w-8", poolReplace)
		createNode(ctx, t, "gpu-w-8", "boot-1")
		createRequest(ctx, t, "extrr-fallback-report", "gpu-w-8", "SysLogsXIDError", "RESTART_BM")
		eventually(t, "the restart to be answered False", func() (bool, string) {
			status, reason := answer(ctx, t, "extrr-fallback-report")
			return status == "False" && reason == controller.AnswerRemediationUnavailable, status + "/" + reason
		})
		if capi.IsMarkedForRemediation(getMachine(ctx, t, "gpu-w-8")) {
			t.Error("a restart no template can carry out marked its Machine")
		}

		// With the Cluster asking for replacements instead, the next one
		// marks its Machine.
		cluster := &clusterv1.Cluster{}
		must(t, hub.Get(ctx, client.ObjectKey{Namespace: namespace, Name: clusterName}, cluster))
		cluster.Annotations = map[string]string{controller.RestartFallbackAnnotation: string(controller.RestartFallbackReplace)}
		must(t, hub.Update(ctx, cluster))

		createMachine(ctx, t, "gpu-w-9", poolReplace)
		createNode(ctx, t, "gpu-w-9", "boot-1")
		createRequest(ctx, t, "extrr-fallback-replace", "gpu-w-9", "SysLogsXIDError", "RESTART_BM")
		eventually(t, "the Machine to be marked for a replacement", func() (bool, string) {
			m := getMachine(ctx, t, "gpu-w-9")
			return capi.IsMarkedForRemediation(m), fmt.Sprint(m.Annotations)
		})
	})

	step("node conditions are left alone where requests are served", func(t *testing.T) {
		if m := getMachine(ctx, t, "gpu-w-5"); capi.IsMarkedForRemediation(m) {
			t.Errorf("a node condition was acted on in request mode: %v", m.Annotations)
		}
	})
}

// eventually polls check until it holds, and fails the test with the last
// state seen when it does not within wait.
func eventually(t *testing.T, what string, check func() (ok bool, state string)) {
	t.Helper()

	deadline := time.Now().Add(wait)
	var state string
	for {
		var ok bool
		if ok, state = check(); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; last seen: %s", what, state)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// consistently polls check for a few poll intervals and fails the test the
// moment it stops holding, for expectations about what must not happen.
func consistently(t *testing.T, what string, check func() (ok bool, state string)) {
	t.Helper()

	deadline := time.Now().Add(4 * pollInterval)
	for time.Now().Before(deadline) {
		if ok, state := check(); !ok {
			t.Fatalf("expected %s; saw: %s", what, state)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// createCluster creates the Cluster, the two health checks and the
// kubeconfig Secret the cluster cache connects with, and reports the
// infrastructure as provisioned, which is what the cluster cache waits for
// before it connects.
func createCluster(ctx context.Context, t *testing.T) {
	t.Helper()

	must(t, hub.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))

	user, err := workloadEnv.AddUser(envtest.User{Name: "remediator", Groups: []string{"system:masters"}}, nil)
	must(t, err)
	kubeconfig, err := user.KubeConfig()
	must(t, err)
	must(t, hub.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      clusterName + "-kubeconfig",
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": kubeconfig},
	}))

	must(t, hub.Create(ctx, healthCheck("gpu-replace", poolReplace)))
	withTemplate := healthCheck("gpu-restart", poolRestart)
	withTemplate.Spec.Remediation.TemplateRef = clusterv1.MachineHealthCheckRemediationTemplateReference{
		APIVersion: "infrastructure.example.com/v1alpha1",
		Kind:       "RebootRemediationTemplate",
		Name:       "reboot",
	}
	must(t, hub.Create(ctx, withTemplate))

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: clusterName},
		// The API server requires a spec, and an empty one is left out of
		// the request.
		Spec: clusterv1.ClusterSpec{ClusterNetwork: clusterv1.ClusterNetwork{ServiceDomain: "cluster.local"}},
	}
	must(t, hub.Create(ctx, cluster))
	cluster.Status.Initialization.InfrastructureProvisioned = ptrTo(true)
	must(t, hub.Status().Update(ctx, cluster))
}

func healthCheck(name, pool string) *clusterv1.MachineHealthCheck {
	return &clusterv1.MachineHealthCheck{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: clusterv1.MachineHealthCheckSpec{
			ClusterName: clusterName,
			Selector:    metav1.LabelSelector{MatchLabels: map[string]string{poolLabel: pool}},
		},
	}
}

// createMachine creates a Machine of the given pool whose node carries the
// same name.
func createMachine(ctx context.Context, t *testing.T, name, pool string) {
	t.Helper()

	m := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName, poolLabel: pool},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: clusterName,
			Bootstrap:   clusterv1.Bootstrap{DataSecretName: ptrTo("bootstrap")},
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: "infrastructure.example.com",
				Kind:     "ExampleMachine",
				Name:     name,
			},
		},
	}
	must(t, hub.Create(ctx, m))
	m.Status.NodeRef = clusterv1.MachineNodeReference{Name: name}
	must(t, hub.Status().Update(ctx, m))
}

// createNode creates a Ready node with the given boot ID and conditions.
func createNode(ctx context.Context, t *testing.T, name, bootID string, conds ...corev1.NodeCondition) {
	t.Helper()

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	must(t, workload.Create(ctx, node))
	node.Status.NodeInfo.BootID = bootID
	node.Status.Conditions = append([]corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}, conds...)
	must(t, workload.Status().Update(ctx, node))
}

func setBootID(ctx context.Context, t *testing.T, name, bootID string) {
	t.Helper()

	node := &corev1.Node{}
	must(t, workload.Get(ctx, client.ObjectKey{Name: name}, node))
	node.Status.NodeInfo.BootID = bootID
	must(t, workload.Status().Update(ctx, node))
}

// createRequest creates an ExternalRemediationRequest and brings it to the
// state NVSentinel's janitor leaves it in once it has released the node.
func createRequest(ctx context.Context, t *testing.T, name, node, check, action string) {
	t.Helper()

	req := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"healthEvent": map[string]any{
			"nodeName":          node,
			"agent":             "syslog-health-monitor",
			"checkName":         check,
			"isFatal":           true,
			"recommendedAction": action,
			"errorCode":         []any{"79"},
		}},
	}}
	req.SetGroupVersionKind(extrr.GroupVersionKind)
	req.SetName(name)
	// A request is answered for good on the poll that first sees it, so the
	// manager's caches are given time to hold the Machine and node created
	// for it just before.
	time.Sleep(2 * pollInterval)
	must(t, workload.Create(ctx, req))

	now := metav1.Now().UTC().Format(time.RFC3339)
	condition := func(conditionType, status string) map[string]any {
		return map[string]any{
			"type": conditionType, "status": status, "reason": "Test", "message": "", "lastTransitionTime": now,
		}
	}
	must(t, unstructured.SetNestedSlice(req.Object, []any{
		condition(extrr.ConditionOwnershipReleased, "True"),
		condition(extrr.ConditionComplete, "Unknown"),
	}, "status", "conditions"))
	must(t, workload.Status().Update(ctx, req))
}

// answer returns the status and reason of the request's completion
// condition.
func answer(ctx context.Context, t *testing.T, name string) (status, reason string) {
	t.Helper()

	req := &unstructured.Unstructured{}
	req.SetGroupVersionKind(extrr.GroupVersionKind)
	must(t, workload.Get(ctx, client.ObjectKey{Name: name}, req))

	conditions, _, _ := unstructured.NestedSlice(req.Object, "status", "conditions")
	for _, c := range conditions {
		condition, _ := c.(map[string]any)
		if condition["type"] == extrr.ConditionComplete {
			status, _ = condition["status"].(string)
			reason, _ = condition["reason"].(string)
		}
	}

	return status, reason
}

func getMachine(ctx context.Context, t *testing.T, name string) *clusterv1.Machine {
	t.Helper()

	m := &clusterv1.Machine{}
	must(t, hub.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, m))

	return m
}

// countEvents counts the Events with the given reason about the named
// object of the given kind.
func countEvents(ctx context.Context, t *testing.T, kind, name, reason string) int {
	t.Helper()

	list := &eventsv1.EventList{}
	must(t, hub.List(ctx, list, client.InNamespace(namespace)))
	n := 0
	for _, e := range list.Items {
		if e.Regarding.Kind == kind && e.Regarding.Name == name && e.Reason == reason {
			n++
		}
	}

	return n
}

func hasEvent(ctx context.Context, t *testing.T, kind, name, reason string) (bool, string) {
	t.Helper()

	n := countEvents(ctx, t, kind, name, reason)

	return n > 0, fmt.Sprintf("%d Events", n)
}

func ptrTo[T any](v T) *T { return &v }

func must(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}
