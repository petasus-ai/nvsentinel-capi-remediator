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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/capi"
)

// restartedFor marks the Machine the way this operator does when it asks
// for a restart on behalf of a request, with the node's boot ID then, and
// gives it the condition a check has left on a Machine it restarted.
func restartedFor(request, bootID string) func(*clusterv1.Machine) {
	return func(m *clusterv1.Machine) {
		markedFor(capi.ActionRestart)(m)
		m.Annotations[capi.RemediationBootIDAnnotation] = bootID
		m.Annotations[capi.RemediationRequestAnnotation] = request
		checked(true)(m)
	}
}

// checked sets the Machine's HealthCheckSucceeded condition the way a
// MachineHealthCheck that remediates through a template does: False when it
// sees the remediate-machine annotation, and True when it finds the Machine
// without it and deletes the remediation request it held.
func checked(sawTheMark bool) func(*clusterv1.Machine) {
	return func(m *clusterv1.Machine) {
		c := metav1.Condition{Type: clusterv1.MachineHealthCheckSucceededCondition, Status: metav1.ConditionTrue, Reason: "Succeeded"}
		if sawTheMark {
			c.Status, c.Reason = metav1.ConditionFalse, clusterv1.MachineHealthCheckHasRemediateAnnotationReason
		}
		meta.SetStatusCondition(&m.Status.Conditions, c)
	}
}

// check has the MachineHealthCheck look at the Machine again, with a
// remediation request of its own to drop if the Machine is unmarked.
func check(t *testing.T, f *fixture, machine string) {
	t.Helper()

	m := f.machine(t, machine)
	checked(capi.IsMarkedForRemediation(m))(m)
	if err := f.hub.Update(context.Background(), m); err != nil {
		t.Fatalf("update machine: %v", err)
	}
}

// restartFixture is a cluster that serves requests and whose checks restart,
// with one Machine and its node.
func restartFixture(t *testing.T, opts fixtureOptions, machine *clusterv1.Machine, node *corev1.Node, requests ...client.Object) *fixture {
	t.Helper()

	opts.healthChecks = []*clusterv1.MachineHealthCheck{newHealthCheck("reboot", withTemplate)}
	opts.workloadMapper = requestMode()
	opts.workloadObjs = requests

	return newFixture(t, opts, []client.Object{newCluster(), machine}, node)
}

// reboot gives the node another boot ID, Ready as before.
func reboot(t *testing.T, f *fixture, node *corev1.Node, bootID string) {
	t.Helper()

	node.Status.NodeInfo.BootID = bootID
	if err := f.workload.Status().Update(context.Background(), node); err != nil {
		t.Fatalf("update node: %v", err)
	}
}

// assertRestartAskedFor checks that the Machine is marked for a restart on
// behalf of the request, with the boot ID recorded.
func assertRestartAskedFor(t *testing.T, f *fixture, machine, request, bootID string) {
	t.Helper()

	m := f.machine(t, machine)
	action, _ := capi.MarkedAction(m)
	if action != capi.ActionRestart || m.Annotations[capi.RemediationRequestAnnotation] != request ||
		m.Annotations[capi.RemediationBootIDAnnotation] != bootID {
		t.Fatalf("mark = %v, want a restart asked for by %s at boot %s", m.Annotations, request, bootID)
	}
}

func TestReconcileRecordsTheRequestARestartIsAskedFor(t *testing.T) {
	f := restartFixture(t, fixtureOptions{}, newMachine("gpu-w-1", "gpu-w-1"), bootedNode("gpu-w-1", "boot-1"),
		pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))

	assertPoll(t, f.reconcile(t))
	assertRestartAskedFor(t, f, "gpu-w-1", "extrr-1", "boot-1")
}

// A replacement is never released, so there is nothing to credit it to.
func TestReconcileRecordsNoRequestWithAReplacement(t *testing.T) {
	f := restartFixture(t, fixtureOptions{}, newMachine("gpu-w-1", "gpu-w-1"), bootedNode("gpu-w-1", "boot-1"),
		pendingRequest("extrr-1", "gpu-w-1", "SysLogsNICDriverError", "REPLACE_VM"))

	assertPoll(t, f.reconcile(t))
	m := f.machine(t, "gpu-w-1")
	if action, _ := capi.MarkedAction(m); action != capi.ActionRemediate {
		t.Fatalf("MarkedAction = %q, want a replacement", action)
	}
	if request, ok := m.Annotations[capi.RemediationRequestAnnotation]; ok {
		t.Fatalf("recorded the request %q with a replacement", request)
	}
}

// The mark of a restart that is over is still on the Machine: its release
// failed, or the operator restarted between the answer and the release. A
// request that arrives then was raised for a fault that restart did not
// cure. It is not answered with that restart; the mark is released and the
// request gets a restart of its own.
func TestReconcileDoesNotAnswerARequestWithAnotherRequestsRestart(t *testing.T) {
	node := bootedNode("gpu-w-1", "boot-2")
	f := restartFixture(t, fixtureOptions{},
		newMachine("gpu-w-1", "gpu-w-1", restartedFor("extrr-1", "boot-1")), node,
		answered(pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "RESTART_BM")),
		pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))

	// The leftover mark is released and the new request waits.
	assertPoll(t, f.reconcile(t))
	if status, _ := f.requestAnswer(t, "extrr-2"); status != "Unknown" {
		t.Fatalf("extrr-2 answered %q with the restart extrr-1 asked for", status)
	}
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("the mark of the finished restart was not released")
	}
	assertEvents(t, f.events(),
		"Normal RemediationReleased Released from remediation: the restart of node gpu-w-1 that extrr-1 asked for is over, and extrr-2 waits for one of its own")

	// The checks have not looked since: a mark set now would be taken for
	// the one just removed, so the request keeps waiting.
	assertPoll(t, f.reconcile(t))
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("Machine marked again before the checks saw it unmarked")
	}
	assertEvents(t, f.events())

	// Once they have, the request gets a restart of its own, from the boot
	// the node is on now.
	check(t, f, "gpu-w-1")
	assertPoll(t, f.reconcile(t))
	assertRestartAskedFor(t, f, "gpu-w-1", "extrr-2", "boot-2")
	assertEvents(t, f.events(), "Warning MarkedForRemediation")
	if status, _ := f.requestAnswer(t, "extrr-2"); status != "Unknown" {
		t.Fatalf("extrr-2 answered %q before its restart", status)
	}

	// Which answers it once the node is back.
	reboot(t, f, node, "boot-3")
	assertPoll(t, f.reconcile(t))
	if status, reason := f.requestAnswer(t, "extrr-2"); status != "True" || reason != AnswerRestarted {
		t.Fatalf("extrr-2 answer = %s %s, want True %s", status, reason, AnswerRestarted)
	}
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("Machine still marked after its restart was answered")
	}
}

// The same, from the failed release on: the request that asked is answered,
// the release fails, and a new request arrives before the next poll.
func TestReconcileDoesNotAnswerANewRequestAfterAFailedRelease(t *testing.T) {
	failing := true
	f := restartFixture(t, fixtureOptions{hubFuncs: interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if failing {
				return errors.New("webhook denied the patch")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}}, newMachine("gpu-w-1", "gpu-w-1", restartedFor("extrr-1", "boot-1")), bootedNode("gpu-w-1", "boot-2"),
		pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))

	assertPoll(t, f.reconcile(t))
	if status, _ := f.requestAnswer(t, "extrr-1"); status != "True" {
		t.Fatalf("extrr-1 answer = %q, want True", status)
	}
	assertRestartAskedFor(t, f, "gpu-w-1", "extrr-1", "boot-1")

	failing = false
	if err := f.workload.Create(context.Background(), pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM")); err != nil {
		t.Fatalf("create request: %v", err)
	}
	assertPoll(t, f.reconcile(t))
	if status, _ := f.requestAnswer(t, "extrr-2"); status != "Unknown" {
		t.Fatalf("extrr-2 answered %q with the restart extrr-1 asked for", status)
	}
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("the mark of the finished restart was not released, or the Machine was marked again in the same poll")
	}

	check(t, f, "gpu-w-1")
	assertPoll(t, f.reconcile(t))
	assertRestartAskedFor(t, f, "gpu-w-1", "extrr-2", "boot-2")
}

// While the restart is still under way its mark is held by whichever
// request waits on the node: releasing it would have the node restarted
// again before it is back.
func TestReconcileHoldsARestartUnderWayForAnotherRequest(t *testing.T) {
	node := bootedNode("gpu-w-1", "boot-1")
	f := restartFixture(t, fixtureOptions{}, newMachine("gpu-w-1", "gpu-w-1", restartedFor("extrr-1", "boot-1")), node,
		pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))

	assertPoll(t, f.reconcile(t))
	assertRestartAskedFor(t, f, "gpu-w-1", "extrr-1", "boot-1")
	if status, _ := f.requestAnswer(t, "extrr-2"); status != "Unknown" {
		t.Fatalf("extrr-2 answered %q while the node has not restarted", status)
	}
	assertEvents(t, f.events())

	// Once the node is back the restart is over, and it was not this
	// request's.
	reboot(t, f, node, "boot-2")
	assertPoll(t, f.reconcile(t))
	if status, _ := f.requestAnswer(t, "extrr-2"); status != "Unknown" {
		t.Fatalf("extrr-2 answered %q with the restart extrr-1 asked for", status)
	}
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("the mark of the finished restart was not released")
	}
}

// A mark that names no request was made before requests were recorded, and
// is credited as it was then.
func TestReconcileAnswersWithARestartThatNamesNoRequest(t *testing.T) {
	f := restartFixture(t, fixtureOptions{}, newMachine("gpu-w-1", "gpu-w-1", restartedFor("", "boot-1"), func(m *clusterv1.Machine) {
		delete(m.Annotations, capi.RemediationRequestAnnotation)
	}), bootedNode("gpu-w-1", "boot-2"),
		pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))

	assertPoll(t, f.reconcile(t))
	if status, reason := f.requestAnswer(t, "extrr-1"); status != "True" || reason != AnswerRestarted {
		t.Fatalf("answer = %s %s, want True %s", status, reason, AnswerRestarted)
	}
}

// Cluster API writes to the Machine as its node comes back, which is when
// the release after the answer is written.
func TestReconcileReleasesAnAnsweredRestartDespiteAConflict(t *testing.T) {
	// The release is refused for real: the Machine in the cluster is newer
	// than the one that was read, so only reading it again gets through.
	t.Run("the Machine changed otherwise", func(t *testing.T) {
		changed := false
		f := restartFixture(t, fixtureOptions{hubFuncs: interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if !changed {
					changed = true
					current := &clusterv1.Machine{}
					if err := c.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
						return err
					}
					current.Labels["touched-by"] = "cluster-api"
					if err := c.Update(ctx, current); err != nil {
						return err
					}
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}}, newMachine("gpu-w-1", "gpu-w-1", restartedFor("extrr-1", "boot-1")), bootedNode("gpu-w-1", "boot-2"),
			pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))

		assertPoll(t, f.reconcile(t))
		if status, _ := f.requestAnswer(t, "extrr-1"); status != "True" {
			t.Fatalf("answer = %q, want True", status)
		}
		m := f.machine(t, "gpu-w-1")
		if capi.IsMarkedForRemediation(m) {
			t.Fatal("Machine not released in the poll that answered")
		}
		if m.Labels["touched-by"] != "cluster-api" {
			t.Fatal("the release undid the other write")
		}
		assertEvents(t, f.events(), "Normal ExternalRemediationRequestAnswered", "Normal RemediationReleased")
	})

	// The cache never catches up: the release gives up after a few tries,
	// and the mark is left to the next poll.
	t.Run("the conflict stays", func(t *testing.T) {
		patches := 0
		f := restartFixture(t, fixtureOptions{hubFuncs: interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				patches++
				return apierrors.NewConflict(clusterv1.GroupVersion.WithResource("machines").GroupResource(), obj.GetName(), errors.New("the object has been modified"))
			},
		}}, newMachine("gpu-w-1", "gpu-w-1", restartedFor("extrr-1", "boot-1")), bootedNode("gpu-w-1", "boot-2"),
			pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))

		assertPoll(t, f.reconcile(t))
		if status, _ := f.requestAnswer(t, "extrr-1"); status != "True" {
			t.Fatalf("answer = %q, want True", status)
		}
		assertRestartAskedFor(t, f, "gpu-w-1", "extrr-1", "boot-1")
		if patches < 2 || patches > 5 {
			t.Fatalf("the release was tried %d times, want a few", patches)
		}
	})
}

// Someone changed the mark between the read and the release: it is no
// longer the restart that was answered, and not this release's to remove.
// The Machine handed in is brought up to date all the same.
func TestReleaseAnsweredLeavesAMarkThatChanged(t *testing.T) {
	for name, change := range map[string]func(*clusterv1.Machine){
		"it became a replacement": func(m *clusterv1.Machine) {
			m.Annotations[capi.RemediationActionAnnotation] = capi.ActionRemediate
		},
		"it is for another boot": func(m *clusterv1.Machine) {
			m.Annotations[capi.RemediationBootIDAnnotation] = "boot-2"
		},
		"it is for another request": func(m *clusterv1.Machine) {
			m.Annotations[capi.RemediationRequestAnnotation] = "extrr-9"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := restartFixture(t, fixtureOptions{}, newMachine("gpu-w-1", "gpu-w-1", restartedFor("extrr-1", "boot-1")), bootedNode("gpu-w-1", "boot-2"))
			read := f.machine(t, "gpu-w-1")
			newer := f.machine(t, "gpu-w-1")
			change(newer)
			if err := f.hub.Update(context.Background(), newer); err != nil {
				t.Fatalf("update machine: %v", err)
			}

			if err := f.r.releaseAnswered(context.Background(), read, "the restart is over"); err != nil {
				t.Fatalf("releaseAnswered: %v", err)
			}
			stored := f.machine(t, "gpu-w-1")
			if restartMarkOf(stored) != restartMarkOf(newer) {
				t.Fatalf("mark = %+v, want it left as it was changed to, %+v", restartMarkOf(stored), restartMarkOf(newer))
			}
			if read.ResourceVersion != stored.ResourceVersion {
				t.Errorf("the Machine handed in is at %s, want it brought up to %s", read.ResourceVersion, stored.ResourceVersion)
			}
			assertEvents(t, f.events())
		})
	}
}

// A Machine that is gone has nothing left to release, whether that shows at
// the release or when the Machine is read again after a conflict.
func TestReleaseAnsweredOfAMachineThatIsGone(t *testing.T) {
	machines := clusterv1.GroupVersion.WithResource("machines").GroupResource()

	t.Run("at the release", func(t *testing.T) {
		f := restartFixture(t, fixtureOptions{}, newMachine("gpu-w-1", "gpu-w-1", restartedFor("extrr-1", "boot-1")), bootedNode("gpu-w-1", "boot-2"))
		read := f.machine(t, "gpu-w-1")
		if err := f.hub.Delete(context.Background(), f.machine(t, "gpu-w-1")); err != nil {
			t.Fatalf("delete machine: %v", err)
		}

		if err := f.r.releaseAnswered(context.Background(), read, "the restart is over"); err != nil {
			t.Fatalf("releaseAnswered: %v", err)
		}
	})

	t.Run("after a conflict", func(t *testing.T) {
		f := restartFixture(t, fixtureOptions{hubFuncs: interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				if err := c.Delete(ctx, obj); err != nil {
					return err
				}
				return apierrors.NewConflict(machines, obj.GetName(), errors.New("the object has been modified"))
			},
		}}, newMachine("gpu-w-1", "gpu-w-1", restartedFor("extrr-1", "boot-1")), bootedNode("gpu-w-1", "boot-2"))

		if err := f.r.releaseAnswered(context.Background(), f.machine(t, "gpu-w-1"), "the restart is over"); err != nil {
			t.Fatalf("releaseAnswered: %v", err)
		}
	})
}

// The checks look at a Machine at intervals. Until they have seen it without
// the mark it carried, a restart asked for anew would be counted against the
// one before, so the request waits; a replacement is where that would end
// anyway and does not wait.
func TestReconcileAsksForARestartOnlyOnceTheChecksForgotTheEarlierMark(t *testing.T) {
	t.Run("a restart waits", func(t *testing.T) {
		f := restartFixture(t, fixtureOptions{}, newMachine("gpu-w-1", "gpu-w-1", checked(true)), bootedNode("gpu-w-1", "boot-2"),
			pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))

		assertPoll(t, f.reconcile(t))
		assertPoll(t, f.reconcile(t))
		if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
			t.Fatal("Machine marked before the checks saw it unmarked")
		}
		if status, _ := f.requestAnswer(t, "extrr-2"); status != "Unknown" {
			t.Fatalf("the waiting request was answered %q", status)
		}
		assertEvents(t, f.events())

		check(t, f, "gpu-w-1")
		assertPoll(t, f.reconcile(t))
		assertRestartAskedFor(t, f, "gpu-w-1", "extrr-2", "boot-2")
	})

	t.Run("a replacement does not", func(t *testing.T) {
		f := restartFixture(t, fixtureOptions{}, newMachine("gpu-w-1", "gpu-w-1", checked(true)), bootedNode("gpu-w-1", "boot-2"),
			pendingRequest("extrr-2", "gpu-w-1", "SysLogsNICDriverError", "REPLACE_VM"))

		assertPoll(t, f.reconcile(t))
		if action, _ := capi.MarkedAction(f.machine(t, "gpu-w-1")); action != capi.ActionRemediate {
			t.Fatalf("MarkedAction = %q, want a replacement", action)
		}
	})

	// Another reason for the condition is not a mark the checks hold on to.
	t.Run("an unhealthy node does not hold a restart back", func(t *testing.T) {
		f := restartFixture(t, fixtureOptions{}, newMachine("gpu-w-1", "gpu-w-1", func(m *clusterv1.Machine) {
			meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
				Type: clusterv1.MachineHealthCheckSucceededCondition, Status: metav1.ConditionFalse, Reason: "UnhealthyNode",
			})
		}), bootedNode("gpu-w-1", "boot-2"), pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))

		assertPoll(t, f.reconcile(t))
		assertRestartAskedFor(t, f, "gpu-w-1", "extrr-2", "boot-2")
	})
}

// waitFixture is restartFixture with a clock the test steps, and a Machine
// that is unmarked while its checks still show an earlier mark.
func waitFixture(t *testing.T, opts fixtureOptions, requests ...client.Object) (*fixture, *time.Time) {
	t.Helper()

	f := restartFixture(t, opts, newMachine("gpu-w-1", "gpu-w-1", checked(true)), bootedNode("gpu-w-1", "boot-2"), requests...)
	now := fixtureTime
	f.r.now = func() time.Time { return now }

	return f, &now
}

// assertHeldBack runs a pass and checks that it left the Machine unmarked.
func assertHeldBack(t *testing.T, f *fixture, when string) {
	t.Helper()

	f.reconcile(t)
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatalf("Machine marked %s", when)
	}
}

// The sign the wait goes by can stay for good: a check that has no
// remediation request to drop leaves the condition as it is. So the wait is
// bounded.
func TestReconcileStopsWaitingForChecksThatDoNotClear(t *testing.T) {
	f, now := waitFixture(t, fixtureOptions{}, pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))

	assertHeldBack(t, f, "when the wait began")
	*now = now.Add(earlierMarkGrace - time.Second)
	assertHeldBack(t, f, "before the wait was over")

	*now = now.Add(time.Second)
	f.reconcile(t)
	assertRestartAskedFor(t, f, "gpu-w-1", "extrr-2", "boot-2")
	if status, _ := f.requestAnswer(t, "extrr-2"); status != "Unknown" {
		t.Fatalf("answer = %q while the restart is pending", status)
	}
}

// The wait is the Machine's: what one signal has waited counts for the
// next, since the checks have had that time whoever was waiting.
func TestReconcileCountsTheTimeAMachineHasWaitedForTheNextRequest(t *testing.T) {
	request := pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM")
	f, now := waitFixture(t, fixtureOptions{}, request)

	assertHeldBack(t, f, "when the wait began")
	if err := f.workload.Delete(context.Background(), request); err != nil {
		t.Fatalf("delete request: %v", err)
	}
	f.reconcile(t)

	*now = now.Add(earlierMarkGrace)
	if err := f.workload.Create(context.Background(), pendingRequest("extrr-3", "gpu-w-1", "SysLogsXIDError", "RESTART_BM")); err != nil {
		t.Fatalf("create request: %v", err)
	}
	f.reconcile(t)
	assertRestartAskedFor(t, f, "gpu-w-1", "extrr-3", "boot-2")
}

// Once the checks have looked the wait is over, and a mark they show later
// is another one: its wait starts from the beginning.
func TestReconcileStartsTheWaitOverOnceTheChecksHaveLooked(t *testing.T) {
	f, now := waitFixture(t, fixtureOptions{}, pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))
	recheck := func(sawTheMark bool) {
		t.Helper()
		m := f.machine(t, "gpu-w-1")
		checked(sawTheMark)(m)
		if err := f.hub.Update(context.Background(), m); err != nil {
			t.Fatalf("update machine: %v", err)
		}
	}

	assertHeldBack(t, f, "when the wait began")
	*now = now.Add(earlierMarkGrace - time.Second)

	// A Machine that is paused is not marked whatever its checks show, so
	// the pass in between asks nothing of the wait.
	m := f.machine(t, "gpu-w-1")
	m.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
	if err := f.hub.Update(context.Background(), m); err != nil {
		t.Fatalf("update machine: %v", err)
	}
	recheck(false)
	f.reconcile(t)

	m = f.machine(t, "gpu-w-1")
	delete(m.Annotations, clusterv1.PausedAnnotation)
	if err := f.hub.Update(context.Background(), m); err != nil {
		t.Fatalf("update machine: %v", err)
	}
	recheck(true)
	*now = now.Add(time.Hour)
	assertHeldBack(t, f, "on the strength of a wait the checks had ended")
	*now = now.Add(earlierMarkGrace - time.Second)
	assertHeldBack(t, f, "before the new wait was over")
}

// A dry run never marks, so the Machine stays as it is after the wait. It
// says once that it would mark, and does not begin to wait again.
func TestReconcileDryRunSettlesAfterTheWait(t *testing.T) {
	f, now := waitFixture(t, fixtureOptions{dryRun: true}, pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))
	skip := func() capi.SkipReason {
		t.Helper()
		f.reconcile(t)
		obs, _ := f.observed("gpu-w-1/SysLogsXIDError")
		return obs.Skip
	}

	if got := skip(); got != capi.SkipEarlierMark {
		t.Fatalf("skip = %q, want the restart held back", got)
	}
	*now = now.Add(earlierMarkGrace)
	for poll := 1; poll <= 3; poll++ {
		if got := skip(); got != "" {
			t.Fatalf("poll %d after the wait: skip = %q, want the dry run past the wait", poll, got)
		}
		*now = now.Add(time.Minute)
	}
}

// A mark that fails when the wait is over is tried again at the next pass,
// not after another wait.
func TestReconcileTriesAFailedMarkAgainWithoutWaitingAgain(t *testing.T) {
	failing := false
	f, now := waitFixture(t, fixtureOptions{hubFuncs: interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if failing {
				return errors.New("webhook denied the patch")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}}, pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))

	assertHeldBack(t, f, "when the wait began")
	*now = now.Add(earlierMarkGrace)
	failing = true
	assertHeldBack(t, f, "although the patch failed")

	failing = false
	f.reconcile(t)
	assertRestartAskedFor(t, f, "gpu-w-1", "extrr-2", "boot-2")
}

// A paused Cluster's checks are paused with it, so the time it spends
// paused is not time they had.
func TestReconcileStartsTheWaitOverAfterTheClusterWasPaused(t *testing.T) {
	f, now := waitFixture(t, fixtureOptions{}, pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))
	pause := func(paused bool) {
		t.Helper()
		c := &clusterv1.Cluster{}
		if err := f.hub.Get(context.Background(), clusterKey, c); err != nil {
			t.Fatalf("get cluster: %v", err)
		}
		c.Spec.Paused = &paused
		if err := f.hub.Update(context.Background(), c); err != nil {
			t.Fatalf("update cluster: %v", err)
		}
	}

	assertHeldBack(t, f, "when the wait began")
	pause(true)
	f.reconcile(t)
	*now = now.Add(10 * time.Minute)
	pause(false)
	assertHeldBack(t, f, "for the time the Cluster was paused")
}

// Nor is the time the workload cluster cannot be reached: the checks cannot
// see its nodes either, and do nothing meanwhile.
func TestReconcileStartsTheWaitOverAfterTheClusterWasOutOfReach(t *testing.T) {
	f, now := waitFixture(t, fixtureOptions{}, pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))

	assertHeldBack(t, f, "when the wait began")
	connected := f.r.ClusterCache
	f.r.ClusterCache = clustercache.NewFakeEmptyClusterCache()
	f.reconcile(t)
	*now = now.Add(10 * time.Minute)
	f.r.ClusterCache = connected
	assertHeldBack(t, f, "for the time the workload cluster was out of reach")
}

// While a restart is held back the Cluster is looked at again when the wait
// is over, should the next poll be later than that.
func TestReconcileComesBackWhenTheWaitIsOver(t *testing.T) {
	f, now := waitFixture(t, fixtureOptions{}, pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))
	f.r.PollInterval = 5 * time.Minute

	if res := f.reconcile(t); res.RequeueAfter != earlierMarkGrace {
		t.Fatalf("requeue after %s while a restart is held back, want %s", res.RequeueAfter, earlierMarkGrace)
	}
	*now = now.Add(earlierMarkGrace)
	if res := f.reconcile(t); res.RequeueAfter != 5*time.Minute {
		t.Fatalf("requeue after %s once nothing is held back, want the poll interval", res.RequeueAfter)
	}
	assertRestartAskedFor(t, f, "gpu-w-1", "extrr-2", "boot-2")
}

// A poll interval shorter than the wait is not stretched by it, and a pass
// woken in the middle of a wait comes back when the wait is over, not a
// whole wait later.
func TestReconcileComesBackNoLaterThanTheWaitNeeds(t *testing.T) {
	t.Run("a short poll interval", func(t *testing.T) {
		f, _ := waitFixture(t, fixtureOptions{}, pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))
		f.r.PollInterval = 10 * time.Second

		if res := f.reconcile(t); res.RequeueAfter != 10*time.Second {
			t.Fatalf("requeue after %s, want the poll interval", res.RequeueAfter)
		}
	})

	t.Run("woken in the middle of the wait", func(t *testing.T) {
		f, now := waitFixture(t, fixtureOptions{}, pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))
		f.r.PollInterval = 5 * time.Minute

		f.reconcile(t)
		*now = now.Add(50 * time.Second)
		if res := f.reconcile(t); res.RequeueAfter != earlierMarkGrace-50*time.Second {
			t.Fatalf("requeue after %s, want the %s that are left of the wait", res.RequeueAfter, earlierMarkGrace-50*time.Second)
		}
	})
}

// A mark this operator made ends the wait before it. Should someone remove
// that mark by hand, the checks have to forget it like any other: the wait
// starts from the beginning.
func TestReconcileStartsTheWaitOverAfterItsOwnMarkWasRemoved(t *testing.T) {
	f, now := waitFixture(t, fixtureOptions{}, pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))

	assertHeldBack(t, f, "when the wait began")
	*now = now.Add(earlierMarkGrace)
	f.reconcile(t)
	assertRestartAskedFor(t, f, "gpu-w-1", "extrr-2", "boot-2")

	m := f.machine(t, "gpu-w-1")
	for _, key := range []string{
		clusterv1.RemediateMachineAnnotation, capi.RemediationReasonAnnotation, capi.RemediationActionAnnotation,
		capi.RemediationBootIDAnnotation, capi.RemediationRequestAnnotation,
	} {
		delete(m.Annotations, key)
	}
	if err := f.hub.Update(context.Background(), m); err != nil {
		t.Fatalf("update machine: %v", err)
	}
	*now = now.Add(time.Hour)
	assertHeldBack(t, f, "on the strength of the wait before its own mark")
}

// Each Machine of each Cluster waits for its own checks.
func TestHeldForChecksKeepsAWaitPerMachineAndCluster(t *testing.T) {
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	r := &ClusterReconciler{now: func() time.Time { return now }}
	one, other := client.ObjectKey{Namespace: "tenant-a", Name: "one"}, client.ObjectKey{Namespace: "tenant-a", Name: "other"}
	first, second := newMachine("gpu-w-1", "gpu-w-1", checked(true)), newMachine("gpu-w-2", "gpu-w-2", checked(true))
	held := func(key client.ObjectKey, m *clusterv1.Machine) bool {
		held, _ := r.heldForChecks(key, m)
		return held
	}

	if !held(one, first) {
		t.Fatal("the first Machine was not held back")
	}
	now = now.Add(earlierMarkGrace - time.Second)
	if !held(one, second) || !held(other, first) {
		t.Fatal("a Machine first seen now was not held back")
	}
	now = now.Add(time.Second)
	if held(one, first) {
		t.Error("the first Machine is still held back after its wait")
	}
	if !held(one, second) {
		t.Error("another Machine of the Cluster was let go with the first")
	}
	if !held(other, first) {
		t.Error("a Machine of the same name in another Cluster was let go with the first")
	}

	// That the bound let a restart through is said once.
	if _, shown := r.heldForChecks(one, first); shown != 0 {
		t.Errorf("shown = %s on a later pass, want it reported once", shown)
	}

	// A Cluster that is gone takes its waits with it.
	r.forget(other)
	now = now.Add(time.Hour)
	if !held(other, first) {
		t.Error("the wait of a Cluster that was forgotten was kept")
	}
}

// The wait is for a restart that can be asked for. One that no check can
// carry out is declined at once, as it always was.
func TestReconcileDeclinesARestartNoCheckCanCarryOutWithoutWaiting(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		workloadMapper: requestMode(),
		workloadObjs:   []client.Object{pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM")},
	}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1", checked(true))}, bootedNode("gpu-w-1", "boot-2"))

	assertPoll(t, f.reconcile(t))
	if status, reason := f.requestAnswer(t, "extrr-2"); status != "False" || reason != AnswerRemediationUnavailable {
		t.Fatalf("answer = %s %s, want False %s", status, reason, AnswerRemediationUnavailable)
	}
}

// The wait is about the Machine and its checks, not about where the signal
// was read or whether anything would be written.
func TestReconcileHoldsEveryRestartBackForTheChecks(t *testing.T) {
	template := []*clusterv1.MachineHealthCheck{newHealthCheck("reboot", withTemplate)}
	condition := func() *corev1.Node {
		n := bootedNode("gpu-w-1", "boot-2")
		n.Status.Conditions = append(n.Status.Conditions, raised("GpuFabricError", restartMessage))
		return n
	}

	t.Run("asked for by a node condition", func(t *testing.T) {
		f := newFixture(t, fixtureOptions{healthChecks: template},
			[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1", checked(true))}, condition())

		assertPoll(t, f.reconcile(t))
		if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
			t.Fatal("Machine marked while its checks still show the earlier mark")
		}
		if obs, _ := f.observed("gpu-w-1/GpuFabricError"); obs.Skip != capi.SkipEarlierMark {
			t.Fatalf("observation = %+v, want the restart held back", obs)
		}
	})

	t.Run("in a dry run", func(t *testing.T) {
		f := newFixture(t, fixtureOptions{healthChecks: template, dryRun: true},
			[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1", checked(true))}, condition())

		assertPoll(t, f.reconcile(t))
		if obs, _ := f.observed("gpu-w-1/GpuFabricError"); obs.Skip != capi.SkipEarlierMark {
			t.Fatalf("observation = %+v, want the dry run to wait as a live run does", obs)
		}
	})
}

// Whether the request that asked is still asking is taken from the request
// as it is, not from the list it was found in, which may be behind.
func TestReconcileReleasesALeftoverMarkWhenTheListStillShowsItsRequest(t *testing.T) {
	f := restartFixture(t, fixtureOptions{workloadFuncs: interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			// Read on its own, extrr-1 has been answered already.
			if u, ok := obj.(*unstructured.Unstructured); ok && key.Name == "extrr-1" {
				u.Object["status"] = answered(u).Object["status"]
			}
			return nil
		},
	}}, newMachine("gpu-w-1", "gpu-w-1", restartedFor("extrr-1", "boot-1")), bootedNode("gpu-w-1", "boot-2"),
		pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"),
		pendingRequest("extrr-2", "gpu-w-1", "SysLogsMemoryError", "RESTART_BM"))

	assertPoll(t, f.reconcile(t))
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("the mark of a request that is answered was held on the word of the list")
	}
}

// The request that asked for the restart keeps its mark while its answer
// cannot be written, and is answered by that restart once it can.
func TestReconcileKeepsTheMarkOfARequestWhoseAnswerFailed(t *testing.T) {
	failing := true
	f := restartFixture(t, fixtureOptions{workloadFuncs: interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if failing {
				return errors.New("apiserver unavailable")
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}}, newMachine("gpu-w-1", "gpu-w-1", restartedFor("extrr-1", "boot-1")), bootedNode("gpu-w-1", "boot-2"),
		pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))

	assertPoll(t, f.reconcile(t))
	assertRestartAskedFor(t, f, "gpu-w-1", "extrr-1", "boot-1")
	assertEvents(t, f.events())

	failing = false
	assertPoll(t, f.reconcile(t))
	if status, reason := f.requestAnswer(t, "extrr-1"); status != "True" || reason != AnswerRestarted {
		t.Fatalf("answer = %s %s, want True %s", status, reason, AnswerRestarted)
	}
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("Machine still marked after the answer")
	}
	assertEvents(t, f.events(), "Normal ExternalRemediationRequestAnswered", "Normal RemediationReleased")
}

// NVSentinel releases a node to one request at a time, so two pending
// requests for one node should not be seen. If they are, the one that did
// not ask for the restart must not cost the one that did its mark while
// that one is still to be answered.
func TestReconcileKeepsTheMarkWhileTheRequestThatAskedIsStillAsking(t *testing.T) {
	failing := true
	f := restartFixture(t, fixtureOptions{workloadFuncs: interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if failing {
				return errors.New("apiserver unavailable")
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}}, newMachine("gpu-w-1", "gpu-w-1", restartedFor("extrr-1", "boot-1")), bootedNode("gpu-w-1", "boot-2"),
		// The same node and check, so the second replaces the first in what
		// is remembered of the pass.
		pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"),
		pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))

	assertPoll(t, f.reconcile(t))
	assertRestartAskedFor(t, f, "gpu-w-1", "extrr-1", "boot-1")

	failing = false
	assertPoll(t, f.reconcile(t))
	if status, _ := f.requestAnswer(t, "extrr-1"); status != "True" {
		t.Fatalf("extrr-1 answer = %q, want True", status)
	}
	if status, _ := f.requestAnswer(t, "extrr-2"); status != "Unknown" {
		t.Fatalf("extrr-2 answered %q with the restart extrr-1 asked for", status)
	}
	// Nor is the Machine marked for it in the pass that released extrr-1's
	// mark: the checks have yet to look.
	if capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
		t.Fatal("Machine marked again in the pass that released it")
	}
}

// Should several requests wait on a leftover mark, the record of its release
// names the first of them, the same one every time.
func TestReconcileNamesTheFirstRequestWaitingOnALeftoverMark(t *testing.T) {
	f := restartFixture(t, fixtureOptions{}, newMachine("gpu-w-1", "gpu-w-1", restartedFor("extrr-1", "boot-1")), bootedNode("gpu-w-1", "boot-2"),
		pendingRequest("extrr-4", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"),
		pendingRequest("extrr-3", "gpu-w-1", "SysLogsMemoryError", "RESTART_BM"),
		pendingRequest("extrr-2", "gpu-w-1", "SysLogsThermalError", "RESTART_BM"))

	f.reconcile(t)
	assertEvents(t, f.events(), "that extrr-1 asked for is over, and extrr-2 waits for one of its own")
}

// A dry run writes nothing, a leftover mark included.
func TestReconcileDryRunLeavesALeftoverMark(t *testing.T) {
	f := restartFixture(t, fixtureOptions{dryRun: true},
		newMachine("gpu-w-1", "gpu-w-1", restartedFor("extrr-1", "boot-1")), bootedNode("gpu-w-1", "boot-2"),
		pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM"))

	assertPoll(t, f.reconcile(t))
	assertPoll(t, f.reconcile(t))
	assertRestartAskedFor(t, f, "gpu-w-1", "extrr-1", "boot-1")
	if status, _ := f.requestAnswer(t, "extrr-2"); status != "Unknown" {
		t.Fatalf("a dry run answered %q", status)
	}
	assertEvents(t, f.events())
	// It says what it would do, which is to release the mark.
	if !f.r.releasableBefore(clusterKey, "gpu-w-1") {
		t.Fatal("the dry run did not find the leftover mark ready to be released")
	}
}

// A request that calls for a replacement holds a restart mark whatever
// restart it was for: a live run turns the mark into the replacement, which
// a dry run cannot, so it must not say it would release it.
func TestReconcileDryRunHoldsALeftoverMarkForAReplacement(t *testing.T) {
	f := restartFixture(t, fixtureOptions{dryRun: true},
		newMachine("gpu-w-1", "gpu-w-1", restartedFor("extrr-1", "boot-1")), bootedNode("gpu-w-1", "boot-2"),
		pendingRequest("extrr-2", "gpu-w-1", "SysLogsNICDriverError", "REPLACE_VM"))

	assertPoll(t, f.reconcile(t))
	if f.r.releasableBefore(clusterKey, "gpu-w-1") {
		t.Fatal("the dry run would release a mark a live run turns into a replacement")
	}
}

// Where a restart falls back to a replacement, a leftover restart mark is
// not released: it is turned into the replacement the request now gets.
func TestReconcileTurnsALeftoverMarkIntoAReplacementUnderTheFallback(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		restartFallback: RestartFallbackReplace,
		workloadMapper:  requestMode(),
		workloadObjs:    []client.Object{pendingRequest("extrr-2", "gpu-w-1", "SysLogsXIDError", "RESTART_BM")},
	}, []client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1", restartedFor("extrr-1", "boot-1"))}, bootedNode("gpu-w-1", "boot-2"))

	assertPoll(t, f.reconcile(t))
	m := f.machine(t, "gpu-w-1")
	if action, _ := capi.MarkedAction(m); action != capi.ActionRemediate {
		t.Fatalf("MarkedAction = %q, want the mark turned into a replacement", action)
	}
	for _, key := range []string{capi.RemediationBootIDAnnotation, capi.RemediationRequestAnnotation} {
		if _, ok := m.Annotations[key]; ok {
			t.Errorf("%s kept on a replacement mark", key)
		}
	}
	if status, _ := f.requestAnswer(t, "extrr-2"); status != "Unknown" {
		t.Fatalf("answer = %q while the replacement is pending", status)
	}
}

// A mark made for a request is held by a node condition like any other
// restart mark when the cluster is read by conditions: there is no request
// to tell apart.
func TestReconcileHoldsARequestsMarkForANodeCondition(t *testing.T) {
	node := bootedNode("gpu-w-1", "boot-2")
	node.Status.Conditions = append(node.Status.Conditions, raised("GpuFabricError", restartMessage))
	f := newFixture(t, fixtureOptions{healthChecks: []*clusterv1.MachineHealthCheck{newHealthCheck("reboot", withTemplate)}},
		[]client.Object{newCluster(), newMachine("gpu-w-1", "gpu-w-1", restartedFor("extrr-1", "boot-1"))}, node)

	assertPoll(t, f.reconcile(t))
	assertRestartAskedFor(t, f, "gpu-w-1", "extrr-1", "boot-1")
}
