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

// Package decision reduces a signal to what this operator will do about it.
//
// NVSentinel recommends one of a fixed set of actions per fault. This operator
// can express three outcomes through Cluster API, so a table maps each
// recommended action to one of them. The table has a built-in default and can
// be overridden entry by entry; it is plain data, and the file format that
// carries overrides is the caller's concern.
package decision

import (
	"fmt"
	"sort"
	"strings"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal"
)

// Decision is what the operator does about a signal.
type Decision string

const (
	// Report records the signal and touches nothing. It is the decision for
	// every action a machine-level operation cannot fix, and the safe default
	// for anything the table does not know.
	Report Decision = "Report"
	// Restart asks the infrastructure provider to restart the node's Machine
	// in place.
	Restart Decision = "Restart"
	// Replace asks Cluster API to delete the Machine and create another.
	Replace Decision = "Replace"
)

// rank orders decisions by how disruptive they are, so that a signal carrying
// several actions resolves to the most disruptive one.
var rank = map[Decision]int{Report: 0, Restart: 1, Replace: 2}

// MoreDisruptiveThan reports whether d outranks o. Callers that see the same
// fault through more than one source use it to keep the outcome that matters.
func (d Decision) MoreDisruptiveThan(o Decision) bool {
	return rank[d] > rank[o]
}

// ParseDecision reads a decision name as written in configuration. Matching
// is case-insensitive; the canonical form is returned.
func ParseDecision(name string) (Decision, error) {
	for _, d := range []Decision{Report, Restart, Replace} {
		if strings.EqualFold(name, string(d)) {
			return d, nil
		}
	}

	return "", fmt.Errorf("unknown decision %q: want one of Report, Restart, Replace", name)
}

// defaults is the built-in table.
//
// REPLACE_VM is the only action that asks for a new node. RESTART_VM and
// RESTART_BM are lifecycle verbs, not a statement about the node type, and
// both mean a restart here. Everything else is either operator escalation
// (CONTACT_SUPPORT, RUN_FIELDDIAG, RUN_DCGMEUD), a GPU-local reset the host
// performs (COMPONENT_RESET), or no action at all. CUSTOM is what a node
// condition shows when the custom action's name was lost in projection; a
// source that knows the name reports the name instead, and the table can map
// it directly.
var defaults = map[string]Decision{
	signal.ActionReplaceVM:      Replace,
	signal.ActionRestartVM:      Restart,
	signal.ActionRestartBM:      Restart,
	signal.ActionNone:           Report,
	signal.ActionComponentReset: Report,
	signal.ActionContactSupport: Report,
	signal.ActionRunFieldDiag:   Report,
	signal.ActionRunDCGMEUD:     Report,
	signal.ActionCustom:         Report,
	signal.ActionUnknown:        Report,
}

// Table maps recommended actions to decisions.
type Table struct {
	entries map[string]Decision
}

// Default returns the built-in table.
func Default() *Table {
	return &Table{entries: copyEntries(defaults)}
}

// New returns the built-in table with the given entries overriding it. Keys
// are recommended action names, including custom ones; values are decision
// names as accepted by ParseDecision. Actions not listed keep their default.
//
// A key that is not an action NVSentinel itself emits is accepted, since that
// is how custom actions are mapped, but it is also how a misspelt built-in
// silently leaves the default in force. Callers should log CustomActions at
// startup so that such an entry is visible.
func New(overrides map[string]string) (*Table, error) {
	t := Default()

	// Sorted so that an error names the same entry every time.
	actions := make([]string, 0, len(overrides))
	for action := range overrides {
		actions = append(actions, action)
	}
	sort.Strings(actions)

	for _, action := range actions {
		name := strings.TrimSpace(action)
		if name == "" {
			return nil, fmt.Errorf("decision table: empty action name")
		}
		d, err := ParseDecision(overrides[action])
		if err != nil {
			return nil, fmt.Errorf("decision table: action %q: %w", name, err)
		}
		t.entries[name] = d
	}

	return t, nil
}

// Entries returns a copy of the table, for logging the effective mapping.
func (t *Table) Entries() map[string]Decision {
	return copyEntries(t.entries)
}

// CustomActions returns, sorted, the entries whose action is not one
// NVSentinel itself emits: custom actions, or misspelt overrides.
func (t *Table) CustomActions() []string {
	var out []string
	for action := range t.entries {
		if !signal.IsKnownAction(action) {
			out = append(out, action)
		}
	}
	sort.Strings(out)

	return out
}

// Lookup returns the decision for one recommended action and whether the
// table has an entry for it. Actions without an entry are reported.
func (t *Table) Lookup(action string) (Decision, bool) {
	d, ok := t.entries[action]
	if !ok {
		return Report, false
	}

	return d, true
}

// Outcome is the result of deciding a signal.
type Outcome struct {
	// Decision is what the operator will do.
	Decision Decision
	// Action is the recommended action that determined the decision. It is
	// empty when the signal carried no action at all.
	Action string
	// Reason explains the outcome in one sentence, for logs and Events.
	Reason string
}

// Decide reduces a signal to one outcome. When the signal carries several
// actions the most disruptive decision wins; among equals the first action
// listed is named. A truncated signal never escalates, since the missing
// actions are unknown, but its reason says so.
func (t *Table) Decide(s signal.Signal) Outcome {
	var (
		best      Outcome
		bestKnown bool
		found     bool
	)

	for _, action := range s.Actions {
		d, known := t.Lookup(action)
		if !found || d.MoreDisruptiveThan(best.Decision) || (known && !bestKnown && d == best.Decision) {
			best = Outcome{Decision: d, Action: action}
			bestKnown = known
			found = true
		}
	}

	switch {
	case !found:
		best = Outcome{Decision: Report, Reason: "the signal carries no recommended action"}
	case bestKnown:
		best.Reason = fmt.Sprintf("%s maps to %s", best.Action, best.Decision)
	default:
		best.Reason = fmt.Sprintf("%s is not in the decision table", best.Action)
	}

	if s.Truncated {
		best.Reason += "; the source dropped trailing events, so a more disruptive action may have been lost"
	}

	return best
}

func copyEntries(in map[string]Decision) map[string]Decision {
	out := make(map[string]Decision, len(in))
	for k, v := range in {
		out[k] = v
	}

	return out
}
