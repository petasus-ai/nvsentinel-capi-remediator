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

package signal

import "testing"

func TestKeyIsOriginIndependent(t *testing.T) {
	fromCondition := Signal{Origin: OriginNodeCondition, Node: "node-1", Check: "GpuXidWatch"}
	fromRequest := Signal{Origin: OriginExternalRemediationRequest, Node: "node-1", Check: "GpuXidWatch", ID: "evt-42"}

	if fromCondition.Key() != fromRequest.Key() {
		t.Errorf("the same fault must key identically from both sources: %q vs %q",
			fromCondition.Key(), fromRequest.Key())
	}
	if want := "node-1/GpuXidWatch"; fromCondition.Key() != want {
		t.Errorf("Key() = %q, want %q", fromCondition.Key(), want)
	}
}

func TestKeySeparatesNodesAndChecks(t *testing.T) {
	a := Signal{Node: "node-1", Check: "GpuXidWatch"}
	b := Signal{Node: "node-2", Check: "GpuXidWatch"}
	c := Signal{Node: "node-1", Check: "GpuNvlinkWatch"}

	if a.Key() == b.Key() {
		t.Error("the same check on another node must be a different fault")
	}
	if a.Key() == c.Key() {
		t.Error("another check on the same node must be a different fault")
	}
}

func TestIsKnownAction(t *testing.T) {
	for _, a := range []string{
		ActionNone, ActionComponentReset, ActionContactSupport, ActionRunFieldDiag,
		ActionRestartVM, ActionRestartBM, ActionReplaceVM, ActionRunDCGMEUD,
		ActionCustom, ActionUnknown,
	} {
		if !IsKnownAction(a) {
			t.Errorf("IsKnownAction(%q) = false, want true", a)
		}
	}
	for _, a := range []string{"", "REPL", "RESTART_", "restart_vm", "REPLACE_VM;"} {
		if IsKnownAction(a) {
			t.Errorf("IsKnownAction(%q) = true, want false", a)
		}
	}
}
