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

package extrr

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IsPending reads the request and reports whether it is still pending. It
// is how a caller holding a cached view of the requests makes sure it does
// not act on one that was answered or deleted a moment ago; through an
// uncached client it sees the request as it is now. A request that is gone
// is not pending.
func IsPending(ctx context.Context, c client.Reader, name string) (bool, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(GroupVersionKind)
	if err := c.Get(ctx, client.ObjectKey{Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get %s %s: %w", GroupVersionKind.Kind, name, err)
	}

	return Pending(obj), nil
}

// Answer tells NVSentinel the outcome of a request by setting its
// ExternalRemediationComplete condition: True once the node is remediated,
// False when it will not be. On True NVSentinel takes the node back; on
// False it leaves it as it is, so False is only given when nothing is
// going to happen.
//
// The request is read again and only its own condition is replaced. The
// status is patched with the request's resource version, because
// NVSentinel rewrites the whole list of conditions and an answer must not
// undo a change it has not seen; the resulting conflict is returned, and
// the caller answers again on its next poll. A request that is no longer
// pending, answered, gone or being deleted, is left alone and reported as
// not answered. Reading through an uncached client keeps the resource
// version current.
func Answer(ctx context.Context, c client.Client, name string, complete bool, reason, message string) (bool, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(GroupVersionKind)
	if err := c.Get(ctx, client.ObjectKey{Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get %s %s: %w", GroupVersionKind.Kind, name, err)
	}
	if !Pending(obj) {
		return false, nil
	}

	status := "False"
	if complete {
		status = "True"
	}
	answer := map[string]any{
		"type":               ConditionComplete,
		"status":             status,
		"reason":             reason,
		"message":            message,
		"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
		"observedGeneration": obj.GetGeneration(),
	}

	updated := obj.DeepCopy()
	conditions, _, _ := unstructured.NestedSlice(updated.Object, "status", "conditions")
	replaced := false
	for i, cond := range conditions {
		if condition, ok := cond.(map[string]any); ok && condition["type"] == ConditionComplete {
			conditions[i] = answer
			replaced = true
		}
	}
	if !replaced {
		conditions = append(conditions, answer)
	}
	// A pending request has a status object holding its conditions, so
	// this cannot fail.
	_ = unstructured.SetNestedSlice(updated.Object, conditions, "status", "conditions")

	if err := c.Status().Patch(ctx, updated, client.MergeFromWithOptions(obj, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, fmt.Errorf("answer %s %s: %w", GroupVersionKind.Kind, name, err)
	}

	return true, nil
}
