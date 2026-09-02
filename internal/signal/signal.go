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

// Package signal defines the health signal this operator acts on and the
// sources that produce it.
//
// NVSentinel publishes a fault in more than one shape: as a node condition
// written by its Kubernetes platform connector, and as an
// ExternalRemediationRequest carrying the full health event. Both are reduced
// to a Signal here so that everything downstream, the decision table and the
// Cluster API actions, is written once.
package signal

import "context"

// Recommended actions emitted by NVSentinel, mirroring the RecommendedAction
// enum in its health_event.proto.
//
// The _VM and _BM suffixes do not describe the node. NVSentinel carries no
// virtualisation detection; each check hardcodes its action, and the fault
// remediation defaults map the restart variants to the same operation. Read
// them as node lifecycle verbs: restart this node, replace this node.
const (
	ActionNone           = "NONE"
	ActionComponentReset = "COMPONENT_RESET"
	ActionContactSupport = "CONTACT_SUPPORT"
	ActionRunFieldDiag   = "RUN_FIELDDIAG"
	ActionRestartVM      = "RESTART_VM"
	ActionRestartBM      = "RESTART_BM"
	ActionReplaceVM      = "REPLACE_VM"
	ActionRunDCGMEUD     = "RUN_DCGMEUD"
	ActionCustom         = "CUSTOM"
	ActionUnknown        = "UNKNOWN"
)

// knownActions is the set of RecommendedAction names NVSentinel can emit.
var knownActions = map[string]struct{}{
	ActionNone: {}, ActionComponentReset: {}, ActionContactSupport: {},
	ActionRunFieldDiag: {}, ActionRestartVM: {}, ActionRestartBM: {},
	ActionReplaceVM: {}, ActionRunDCGMEUD: {}, ActionCustom: {}, ActionUnknown: {},
}

// IsKnownAction reports whether name is a RecommendedAction NVSentinel can
// emit. Sources use it to tell a complete action from a cut-off one.
func IsKnownAction(name string) bool {
	_, ok := knownActions[name]
	return ok
}

// Origin records which kind of source produced a Signal.
type Origin string

const (
	// OriginNodeCondition marks a signal decoded from a node condition.
	OriginNodeCondition Origin = "NodeCondition"
	// OriginExternalRemediationRequest marks a signal read from an
	// ExternalRemediationRequest.
	OriginExternalRemediationRequest Origin = "ExternalRemediationRequest"
)

// Signal is one fault NVSentinel raised for a node, decoded into the fields a
// remediation decision needs.
type Signal struct {
	// Origin is the kind of source the signal came from.
	Origin Origin
	// Node is the name of the workload cluster node the signal is about.
	Node string
	// Check names the NVSentinel check that raised the signal: the node
	// condition type, or the health event's check name.
	Check string
	// ID identifies the underlying health event when the source carries one.
	// Node conditions do not, so it may be empty.
	ID string
	// Actions holds every recommended action found in the signal. A single
	// node condition can carry several concatenated events.
	Actions []string
	// ErrorCodes holds the DCGM or syslog error codes, e.g. DCGM_FR_XID_ERROR.
	ErrorCodes []string
	// GpuUUIDs identifies the affected devices when the event names them.
	GpuUUIDs []string
	// Truncated reports that the source dropped trailing events for this
	// check, so Actions may be missing the most disruptive one.
	Truncated bool
}

// Key identifies the fault a Signal reports, independently of which source
// observed it, so that the same fault seen through a node condition and
// through an ExternalRemediationRequest is handled once. A check raises one
// condition per node, and an ExternalRemediationRequest captures one health
// event, so node and check name identify both.
func (s Signal) Key() string {
	return s.Node + "/" + s.Check
}

// Source lists the signals currently raised in one workload cluster.
type Source interface {
	// Name identifies the source in logs and Events.
	Name() string
	// Collect returns every signal currently raised. Signals for healthy
	// nodes are not returned.
	Collect(ctx context.Context) ([]Signal, error)
}
