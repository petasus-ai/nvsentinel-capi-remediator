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

// Package extrr reads NVSentinel's ExternalRemediationRequests.
//
// NVSentinel hands a node to an external remediation system by creating an
// ExternalRemediationRequest for it: its fault remediation renders the
// request from a template configured for a recommended action, and its
// janitor then marks the node as no longer NVSentinel's to manage and sets
// the NVSentinelOwnershipReleased condition. From that moment the node is
// the external system's until it reports back through the
// ExternalRemediationComplete condition. The request carries the health
// event itself, so none of it has to be parsed out of text.
//
// The requests are read as unstructured objects. NVSentinel defines their
// Go type only inside its janitor module: importing it would raise k8s.io/*
// to v0.37 and controller-runtime to v0.25, past the Cluster API release
// this operator follows, and its sibling modules resolve only through the
// janitor module's own replace directives, so it cannot be required from
// outside at all.
package extrr

import (
	"context"
	"fmt"
	"math"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal"
)

// SourceName identifies the ExternalRemediationRequest source in logs and
// Events.
const SourceName = "ExternalRemediationRequest"

// GroupVersionKind is the served version of ExternalRemediationRequest. The
// resource is cluster-scoped and has a status subresource.
var GroupVersionKind = schema.GroupVersionKind{
	Group:   "nvsentinel.dgxc.nvidia.com",
	Version: "v1",
	Kind:    "ExternalRemediationRequest",
}

// Condition types on a request's status.
const (
	// ConditionOwnershipReleased is set True by NVSentinel once it has
	// released the node to the external system.
	ConditionOwnershipReleased = "NVSentinelOwnershipReleased"
	// ConditionComplete is the external system's answer: True once the
	// remediation is done, False when it will not be done. NVSentinel sets
	// it Unknown while it waits.
	ConditionComplete = "ExternalRemediationComplete"
)

// gpuUUIDEntity is the entity type NVSentinel uses for a GPU's UUID among
// the entities a health event names.
const gpuUUIDEntity = "GPU_UUID"

// actionNames maps the numeric values of NVSentinel's RecommendedAction
// enum, from its health_event.proto, to their names. The CRD accepts
// either form, and a request rendered from a template may carry either.
var actionNames = map[int64]string{
	0:  signal.ActionNone,
	2:  signal.ActionComponentReset,
	5:  signal.ActionContactSupport,
	6:  signal.ActionRunFieldDiag,
	15: signal.ActionRestartVM,
	24: signal.ActionRestartBM,
	25: signal.ActionReplaceVM,
	26: signal.ActionRunDCGMEUD,
	27: signal.ActionCustom,
	99: signal.ActionUnknown,
}

// Parse decodes the health event of one request. It reports false for a
// request that names no node, which NVSentinel's admission webhook never
// lets through. The check name falls back to the request's name when the
// template rendering the request left it out, which keeps the signal's key
// non-empty and apart from any node condition's.
func Parse(obj *unstructured.Unstructured) (signal.Signal, bool) {
	// Read without copying: the event is only looked at.
	field, _, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "healthEvent")
	event, _ := field.(map[string]any)

	node, _, _ := unstructured.NestedString(event, "nodeName")
	if node == "" {
		return signal.Signal{}, false
	}

	check, _, _ := unstructured.NestedString(event, "checkName")
	if check == "" {
		check = obj.GetName()
	}
	id, _, _ := unstructured.NestedString(event, "id")
	codes, _, _ := unstructured.NestedStringSlice(event, "errorCode")

	sig := signal.Signal{
		Origin:     signal.OriginExternalRemediationRequest,
		Node:       node,
		Check:      check,
		ID:         id,
		ErrorCodes: codes,
		GpuUUIDs:   gpuUUIDs(event),
		Request:    obj.GetName(),
	}
	if action, ok := recommendedAction(event); ok {
		sig.Actions = []string{action}
	}

	return sig, true
}

// recommendedAction returns the event's recommended action by name. A
// custom action is reported by its own name, which is what the decision
// table maps; CUSTOM itself only when the name is missing. A number outside
// the enum, which the CRD's validation rejects anyway, is UNKNOWN.
func recommendedAction(event map[string]any) (string, bool) {
	var name string
	switch v := event["recommendedAction"].(type) {
	case string:
		name = v
	case int64:
		name = numberedAction(v)
	case float64:
		name = signal.ActionUnknown
		if v == math.Trunc(v) {
			name = numberedAction(int64(v))
		}
	default:
		return "", false
	}

	if name == signal.ActionCustom {
		if custom, _, _ := unstructured.NestedString(event, "customRecommendedAction"); custom != "" {
			return custom, true
		}
	}

	return name, name != ""
}

// numberedAction names the enum value v, or UNKNOWN when it has none.
func numberedAction(v int64) string {
	if name, ok := actionNames[v]; ok {
		return name
	}

	return signal.ActionUnknown
}

// gpuUUIDs returns the UUIDs of the GPUs the event names.
func gpuUUIDs(event map[string]any) []string {
	entities, _ := event["entitiesImpacted"].([]any)

	var out []string
	for _, e := range entities {
		entity, ok := e.(map[string]any)
		if !ok || entity["entityType"] != gpuUUIDEntity {
			continue
		}
		if value, ok := entity["entityValue"].(string); ok && value != "" {
			out = append(out, value)
		}
	}

	return out
}

// Pending reports whether the request is waiting on the external system:
// NVSentinel has released the node, no answer has been given, and the
// request is not being deleted. A request whose node NVSentinel has not
// released yet is not the external system's to act on.
func Pending(obj *unstructured.Unstructured) bool {
	return obj.GetDeletionTimestamp() == nil &&
		ConditionStatus(obj, ConditionOwnershipReleased) == "True" &&
		!Answered(obj)
}

// Answered reports whether the external system has answered the request,
// either way.
func Answered(obj *unstructured.Unstructured) bool {
	status := ConditionStatus(obj, ConditionComplete)
	return status == "True" || status == "False"
}

// ConditionStatus returns the status of the condition of the given type, or
// the empty string when the request does not carry it.
func ConditionStatus(obj *unstructured.Unstructured, conditionType string) string {
	// Read without copying: the conditions are only looked at.
	field, _, _ := unstructured.NestedFieldNoCopy(obj.Object, "status", "conditions")
	conditions, _ := field.([]any)
	for _, c := range conditions {
		condition, ok := c.(map[string]any)
		if !ok || condition["type"] != conditionType {
			continue
		}
		status, _ := condition["status"].(string)
		return status
	}

	return ""
}

// Source reads the pending ExternalRemediationRequests of one workload
// cluster.
type Source struct {
	reader client.Reader
}

var _ signal.Source = (*Source)(nil)

// NewSource returns a Source that lists requests through reader, which is
// expected to be a cached client for the workload cluster so that polling
// it costs nothing on the wire.
func NewSource(reader client.Reader) *Source {
	return &Source{reader: reader}
}

// Name implements signal.Source.
func (s *Source) Name() string {
	return SourceName
}

// Collect returns a signal for every pending request. Requests NVSentinel
// has not released yet, and those already answered, produce nothing.
// NVSentinel releases a node to one request at a time and holds that until
// the request is answered True, so a node has at most one pending request
// and its signal key stays unique. The result is sorted by node, check and
// request so that consecutive polls compare.
func (s *Source) Collect(ctx context.Context) ([]signal.Signal, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(GroupVersionKind.GroupVersion().WithKind(GroupVersionKind.Kind + "List"))
	if err := s.reader.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list %s: %w", GroupVersionKind.Kind, err)
	}

	var out []signal.Signal
	for i := range list.Items {
		obj := &list.Items[i]
		if !Pending(obj) {
			continue
		}
		if sig, ok := Parse(obj); ok {
			out = append(out, sig)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		switch {
		case out[i].Node != out[j].Node:
			return out[i].Node < out[j].Node
		case out[i].Check != out[j].Check:
			return out[i].Check < out[j].Check
		default:
			return out[i].Request < out[j].Request
		}
	})

	return out, nil
}
