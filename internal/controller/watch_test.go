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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/capi"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal/extrr"
)

// watchingCache is a ClusterCache that starts the watches it is asked for on
// fake informers, the way the cluster cache starts them on the cache of a
// connection: a watch of one name once, and only a watch that started
// counts as added. It keeps every call.
type watchingCache struct {
	clustercache.ClusterCache
	informers *informertest.FakeInformers
	// err fails every call while it is set.
	err error

	calls    []clustercache.Watcher
	clusters []client.ObjectKey
	// started counts, per name, the watches that were started.
	started map[string]int
}

func (c *watchingCache) Watch(_ context.Context, cluster client.ObjectKey, w clustercache.Watcher) error {
	c.calls = append(c.calls, w)
	c.clusters = append(c.clusters, cluster)
	if c.err != nil {
		return c.err
	}
	if c.started[w.Name()] > 0 {
		return nil
	}
	if err := w.Watch(c.informers); err != nil {
		return err
	}
	if c.started == nil {
		c.started = map[string]int{}
	}
	c.started[w.Name()]++

	return nil
}

// queueWatcher starts the sources it is given on a queue, as a controller
// does, and waits until they deliver events.
type queueWatcher struct {
	ctx   context.Context
	queue workqueue.TypedRateLimitingInterface[ctrl.Request]
}

func (w queueWatcher) Watch(src source.TypedSource[ctrl.Request]) error {
	if err := src.Start(w.ctx, w.queue); err != nil {
		return err
	}
	if syncing, ok := src.(source.TypedSyncingSource[ctrl.Request]); ok {
		return syncing.WaitForSync(w.ctx)
	}

	return nil
}

// watchFixture is a fixture of a cluster that serves requests, with a cache
// that starts watches and a queue standing in for the controller's.
func watchFixture(t *testing.T, hubObjs []client.Object, requests ...client.Object) (*fixture, *watchingCache, workqueue.TypedRateLimitingInterface[ctrl.Request]) {
	t.Helper()

	f := newFixture(t, fixtureOptions{workloadMapper: requestMode(), workloadObjs: requests},
		append([]client.Object{newCluster()}, hubObjs...), bootedNode("gpu-w-1", "boot-1"))

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[ctrl.Request]())
	t.Cleanup(queue.ShutDown)

	cache := &watchingCache{ClusterCache: f.r.ClusterCache, informers: &informertest.FakeInformers{Scheme: newScheme(t)}}
	f.r.ClusterCache = cache
	f.r.requestWatcher = queueWatcher{ctx: t.Context(), queue: queue}

	return f, cache, queue
}

// unreleased returns a copy of the request the way NVSentinel creates it,
// before its janitor has released the node.
func unreleased(request *unstructured.Unstructured) *unstructured.Unstructured {
	out := request.DeepCopy()
	unstructured.RemoveNestedField(out.Object, "status")

	return out
}

// answered returns a copy of the request with this operator's answer.
func answered(request *unstructured.Unstructured) *unstructured.Unstructured {
	out := request.DeepCopy()
	_ = unstructured.SetNestedSlice(out.Object, []any{
		map[string]any{"type": extrr.ConditionOwnershipReleased, "status": "True"},
		map[string]any{"type": extrr.ConditionComplete, "status": "True"},
	}, "status", "conditions")

	return out
}

func TestRequestsWakeTheClusterWhenTheyBecomePending(t *testing.T) {
	f, cache, queue := watchFixture(t, nil)
	assertPoll(t, f.reconcile(t))

	kind := &unstructured.Unstructured{}
	kind.SetGroupVersionKind(extrr.GroupVersionKind)
	requests, err := cache.informers.FakeInformerFor(t.Context(), kind)
	if err != nil {
		t.Fatalf("the requests' informer: %v", err)
	}
	// woken reports whether the Cluster was queued, and takes it off the
	// queue again.
	woken := func() bool {
		t.Helper()
		if queue.Len() == 0 {
			return false
		}
		got, _ := queue.Get()
		queue.Done(got)
		if got.NamespacedName != clusterKey || queue.Len() != 0 {
			t.Fatalf("queued %v and %d more, want only %v", got, queue.Len(), clusterKey)
		}

		return true
	}

	pending := pendingRequest("extrr-1", "gpu-w-1", "SysLogsXIDError", "RESTART_BM")
	relabelled := pending.DeepCopy()
	relabelled.SetLabels(map[string]string{"touched": "true"})
	deleting := pending.DeepCopy()
	now := metav1.Now()
	deleting.SetDeletionTimestamp(&now)

	steps := []struct {
		name string
		fire func()
		want bool
	}{
		// NVSentinel creates a request before it releases the node to it.
		{"created before the node is released", func() { requests.Add(unreleased(pending)) }, false},
		{"the node is released", func() { requests.Update(unreleased(pending), pending) }, true},
		{"changed while pending", func() { requests.Update(pending, relabelled) }, false},
		{"answered", func() { requests.Update(pending, answered(pending)) }, false},
		{"answered and changed again", func() { requests.Update(answered(pending), answered(relabelled)) }, false},
		// Whoever takes an answer back makes the request this operator's again.
		{"answer withdrawn", func() { requests.Update(answered(pending), pending) }, true},
		{"being deleted", func() { requests.Update(pending, deleting) }, false},
		{"deleted", func() { requests.Delete(pending) }, false},
		// What a watch delivers first for every request there already is.
		{"pending when first seen", func() { requests.Add(pending) }, true},
		{"answered when first seen", func() { requests.Add(answered(pending)) }, false},
	}
	for _, step := range steps {
		step.fire()
		if got := woken(); got != step.want {
			t.Errorf("%s: Cluster queued = %v, want %v", step.name, got, step.want)
		}
	}
}

func TestReconcileWatchesRequestsOnlyWhereTheyAreServed(t *testing.T) {
	t.Run("served", func(t *testing.T) {
		f, cache, _ := watchFixture(t, nil)

		// Every reconcile asks: the cluster cache drops its watches with the
		// connection, and only it knows whether this one is still there.
		assertPoll(t, f.reconcile(t))
		assertPoll(t, f.reconcile(t))
		if len(cache.calls) != 2 {
			t.Fatalf("asked for %d watches in two reconciles, want one each", len(cache.calls))
		}
		for i, w := range cache.calls {
			if w.Name() != requestWatchName || w.Object().GetObjectKind().GroupVersionKind() != extrr.GroupVersionKind {
				t.Errorf("watch %q on %v, want %q on %v", w.Name(), w.Object().GetObjectKind().GroupVersionKind(),
					requestWatchName, extrr.GroupVersionKind)
			}
			if cache.clusters[i] != clusterKey {
				t.Errorf("watch asked for on %v, want %v", cache.clusters[i], clusterKey)
			}
		}
		if cache.started[requestWatchName] != 1 || len(cache.informers.InformersByGVK) != 1 {
			t.Errorf("%d watches on %d informers started, want one on the requests' informer",
				cache.started[requestWatchName], len(cache.informers.InformersByGVK))
		}
	})

	// A watch on a resource that is not served delivers nothing and
	// complains until the resource is served.
	for name, mode := range map[string]fixtureOptions{
		"only node conditions":   {},
		"the janitor remediates": {workloadMapper: newMapper(janitorGroupVersionKind)},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, mode, []client.Object{newCluster()}, bootedNode("gpu-w-1", "boot-1"))
			cache := &watchingCache{ClusterCache: f.r.ClusterCache, informers: &informertest.FakeInformers{Scheme: newScheme(t)}}
			f.r.ClusterCache = cache
			queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[ctrl.Request]())
			t.Cleanup(queue.ShutDown)
			f.r.requestWatcher = queueWatcher{ctx: t.Context(), queue: queue}

			assertPoll(t, f.reconcile(t))
			if len(cache.calls) != 0 {
				t.Fatalf("asked for %d watches where no requests are served", len(cache.calls))
			}
		})
	}
}

// The watch only shortens the wait: without it the requests are still read
// at every poll, and every poll asks for it again.
func TestReconcileActsOnRequestsItCannotWatch(t *testing.T) {
	hubObjs := []client.Object{newMachine("gpu-w-1", "gpu-w-1")}
	request := pendingRequest("extrr-1", "gpu-w-1", "SysLogsNICDriverError", "REPLACE_VM")

	t.Run("the cluster cache refuses", func(t *testing.T) {
		f, cache, _ := watchFixture(t, hubObjs, request)
		cache.err = errors.New("the connection is gone")

		assertPoll(t, f.reconcile(t))
		if len(cache.calls) != 1 || len(cache.started) != 0 {
			t.Fatalf("asked for %d watches and started %d, want 1 and none", len(cache.calls), len(cache.started))
		}
		if !capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
			t.Fatal("the request's Machine was not marked")
		}

		cache.err = nil
		assertPoll(t, f.reconcile(t))
		if len(cache.calls) != 2 || cache.started[requestWatchName] != 1 {
			t.Fatalf("asked for %d watches and started %d, want 2 and 1", len(cache.calls), cache.started[requestWatchName])
		}
	})

	// A reconciler that was never set up with a manager has no controller to
	// wake, and must not hand the cluster cache a watch that would panic.
	t.Run("no controller", func(t *testing.T) {
		f, cache, _ := watchFixture(t, hubObjs, request)
		f.r.requestWatcher = nil

		assertPoll(t, f.reconcile(t))
		if len(cache.calls) != 0 {
			t.Fatalf("asked the cluster cache for %d watches without a controller", len(cache.calls))
		}
		if !capi.IsMarkedForRemediation(f.machine(t, "gpu-w-1")) {
			t.Fatal("the request's Machine was not marked")
		}
	})
}
