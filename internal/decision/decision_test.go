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

package decision

import (
	"reflect"
	"strings"
	"testing"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal/condition"
)

func TestDefaultTableCoversEveryAction(t *testing.T) {
	want := map[string]Decision{
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

	table := Default()
	for action, wantDecision := range want {
		got, ok := table.Lookup(action)
		if !ok {
			t.Errorf("Lookup(%q): no entry, every NVSentinel action needs one", action)
		}
		if got != wantDecision {
			t.Errorf("Lookup(%q) = %s, want %s", action, got, wantDecision)
		}
	}
	if len(want) != len(defaults) {
		t.Errorf("default table has %d entries, test covers %d; keep them in sync", len(defaults), len(want))
	}
}

// TestDecideObservedFaults runs the verbatim node condition messages the
// decoder tests use through the default table, so the whole path from
// message to decision is pinned by real faults.
func TestDecideObservedFaults(t *testing.T) {
	tests := []struct {
		name          string
		conditionType string
		message       string
		want          Decision
	}{
		{
			// The IMEX false positive (NVIDIA/NVSentinel#1471) must never
			// cost a node.
			name:          "imex false positive reports",
			conditionType: "GpuNvlinkWatch",
			message: "ErrorCode:DCGM_FR_IMEX_UNHEALTHY GPU:0 PCI:0000:ef:00.0 " +
				"GPU_UUID:GPU-b5f21324-f1f9-f1a4-1a37-1166234f6e4f IMEX daemon status is -3 (not READY) " +
				"Check IMEX installation, configuration, domain and daemon status, and network connectivity. " +
				"Recommended Action=CONTACT_SUPPORT;",
			want: Report,
		},
		{
			name:          "xid 79 restarts",
			conditionType: "SysLogsXIDError",
			message: "ErrorCode:79 PCI:0000:ef:00 GPU_UUID:GPU-b5f21324-f1f9-f1a4-1a37-1166234f6e4f " +
				"MESSAGE=NVRM: Xid (PCI:0000:ef:00): 79, pid=0, name=<unknown>, " +
				"GPU has fallen off the bus. Recommended Action=RESTART_BM;",
			want: Restart,
		},
		{
			name:          "mlx5 unrecoverable error replaces",
			conditionType: "SysLogsNICDriverError",
			message: "ErrorCode:unrecoverable_err MESSAGE=mlx5_core 0000:c1:00.0: mlx5_crdump_collect: " +
				"unrecoverable hardware error detected Recommended Action=REPLACE_VM;",
			want: Replace,
		},
	}

	table := Default()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, ok := condition.Parse(tt.conditionType, tt.message)
			if !ok {
				t.Fatal("test setup: message not recognised")
			}
			got := table.Decide(s)
			if got.Decision != tt.want {
				t.Errorf("Decide() = %s (%s), want %s", got.Decision, got.Reason, tt.want)
			}
			if got.Action != s.Actions[0] {
				t.Errorf("Action = %q, want %q", got.Action, s.Actions[0])
			}
		})
	}
}

func TestDecidePrecedence(t *testing.T) {
	tests := []struct {
		name       string
		actions    []string
		want       Decision
		wantAction string
	}{
		{"single replace", []string{signal.ActionReplaceVM}, Replace, signal.ActionReplaceVM},
		{"single restart vm", []string{signal.ActionRestartVM}, Restart, signal.ActionRestartVM},
		{"single restart bm", []string{signal.ActionRestartBM}, Restart, signal.ActionRestartBM},
		{"single report", []string{signal.ActionContactSupport}, Report, signal.ActionContactSupport},
		{"most disruptive wins regardless of order",
			[]string{signal.ActionContactSupport, signal.ActionRestartVM, signal.ActionReplaceVM}, Replace, signal.ActionReplaceVM},
		{"restart outranks report", []string{signal.ActionNone, signal.ActionRestartVM}, Restart, signal.ActionRestartVM},
		{"first of equals is named", []string{signal.ActionRestartBM, signal.ActionRestartVM}, Restart, signal.ActionRestartBM},
		{"unknown action reports", []string{"REBOOT_EVERYTHING"}, Report, "REBOOT_EVERYTHING"},
		{"known report beats unknown for naming", []string{"REBOOT_EVERYTHING", signal.ActionContactSupport}, Report, signal.ActionContactSupport},
		{"unknown never outranks a known restart", []string{signal.ActionRestartVM, "REBOOT_EVERYTHING"}, Restart, signal.ActionRestartVM},
	}

	table := Default()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := table.Decide(signal.Signal{Actions: tt.actions})
			if got.Decision != tt.want {
				t.Errorf("Decide(%v) = %s, want %s", tt.actions, got.Decision, tt.want)
			}
			if got.Action != tt.wantAction {
				t.Errorf("Action = %q, want %q", got.Action, tt.wantAction)
			}
			if got.Reason == "" {
				t.Error("every outcome must carry a reason")
			}
		})
	}
}

func TestDecideWithoutActions(t *testing.T) {
	got := Default().Decide(signal.Signal{})
	if got.Decision != Report {
		t.Errorf("Decide() = %s, want %s", got.Decision, Report)
	}
	if got.Action != "" {
		t.Errorf("Action = %q, want empty", got.Action)
	}
	if got.Reason == "" {
		t.Error("every outcome must carry a reason")
	}
}

func TestDecideReasons(t *testing.T) {
	table := Default()

	if got := table.Decide(signal.Signal{Actions: []string{signal.ActionReplaceVM}}); got.Reason != "REPLACE_VM maps to Replace" {
		t.Errorf("Reason = %q", got.Reason)
	}
	if got := table.Decide(signal.Signal{Actions: []string{"REBOOT_EVERYTHING"}}); got.Reason != "REBOOT_EVERYTHING is not in the decision table" {
		t.Errorf("Reason = %q", got.Reason)
	}
}

func TestDecideTruncatedNeverEscalates(t *testing.T) {
	got := Default().Decide(signal.Signal{Actions: []string{signal.ActionContactSupport}, Truncated: true})
	if got.Decision != Report {
		t.Errorf("Decide() = %s, want %s: missing actions are unknown, not assumed", got.Decision, Report)
	}
	if !strings.Contains(got.Reason, "dropped trailing events") {
		t.Errorf("Reason = %q, want a note that the action list may be incomplete", got.Reason)
	}
}

func TestNewOverrides(t *testing.T) {
	table, err := New(map[string]string{
		signal.ActionRestartBM: "replace", // case-insensitive
		"REPLACE_DISK":         "Replace", // custom action reported under its own name
		signal.ActionReplaceVM: "report",  // downgrade
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	tests := map[string]Decision{
		signal.ActionRestartBM:      Replace,
		"REPLACE_DISK":              Replace,
		signal.ActionReplaceVM:      Report,
		signal.ActionRestartVM:      Restart, // untouched default
		signal.ActionContactSupport: Report,  // untouched default
	}
	for action, want := range tests {
		if got, ok := table.Lookup(action); !ok || got != want {
			t.Errorf("Lookup(%q) = %s, %v; want %s, true", action, got, ok, want)
		}
	}

	if got := table.Decide(signal.Signal{Actions: []string{"REPLACE_DISK", signal.ActionReplaceVM}}); got.Decision != Replace || got.Action != "REPLACE_DISK" {
		t.Errorf("Decide() = %s via %q, want %s via REPLACE_DISK", got.Decision, got.Action, Replace)
	}
}

func TestNewLeavesDefaultsUntouched(t *testing.T) {
	if _, err := New(map[string]string{signal.ActionReplaceVM: "Report"}); err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got, _ := Default().Lookup(signal.ActionReplaceVM); got != Replace {
		t.Errorf("overriding one table changed the built-in default: Lookup(REPLACE_VM) = %s", got)
	}
}

func TestNewRejectsBadEntries(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		wantErr   string
	}{
		{"unknown decision", map[string]string{signal.ActionRestartVM: "Reboot"}, "unknown decision"},
		{"empty decision", map[string]string{signal.ActionRestartVM: ""}, "unknown decision"},
		{"empty action", map[string]string{"": "Replace"}, "empty action name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.overrides)
			if err == nil {
				t.Fatal("New() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("New() error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestNewErrorNamesTheEntry(t *testing.T) {
	_, err := New(map[string]string{signal.ActionRestartVM: "Reboot", signal.ActionRestartBM: "Reboot"})
	if err == nil {
		t.Fatal("New() error = nil, want an error")
	}
	// Sorted iteration makes the message deterministic.
	if !strings.Contains(err.Error(), `action "RESTART_BM"`) {
		t.Errorf("New() error = %q, want it to name the first bad entry in sorted order", err)
	}
}

func TestMoreDisruptiveThan(t *testing.T) {
	if !Replace.MoreDisruptiveThan(Restart) || !Restart.MoreDisruptiveThan(Report) || !Replace.MoreDisruptiveThan(Report) {
		t.Error("want Replace > Restart > Report")
	}
	for _, d := range []Decision{Report, Restart, Replace} {
		if d.MoreDisruptiveThan(d) {
			t.Errorf("%s must not outrank itself", d)
		}
	}
	if Report.MoreDisruptiveThan(Restart) || Restart.MoreDisruptiveThan(Replace) {
		t.Error("ordering must not be symmetric")
	}
}

func TestNewTrimsActionNames(t *testing.T) {
	table, err := New(map[string]string{"  REPLACE_VM ": "Report"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got, _ := table.Lookup(signal.ActionReplaceVM); got != Report {
		t.Errorf("a padded key must override the built-in action, Lookup(REPLACE_VM) = %s", got)
	}
	if custom := table.CustomActions(); len(custom) != 0 {
		t.Errorf("CustomActions() = %v, want none: the trimmed key is a built-in action", custom)
	}
}

func TestEntriesAndCustomActions(t *testing.T) {
	table, err := New(map[string]string{
		"REPLACE_DISK":         "Replace",
		"REPLACE_VN":           "Report", // misspelt REPLACE_VM: must be visible
		signal.ActionRestartBM: "Replace",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if got, want := table.CustomActions(), []string{"REPLACE_DISK", "REPLACE_VN"}; !reflect.DeepEqual(got, want) {
		t.Errorf("CustomActions() = %v, want %v", got, want)
	}
	if got := Default().CustomActions(); len(got) != 0 {
		t.Errorf("the default table has no custom actions, got %v", got)
	}

	entries := table.Entries()
	if entries[signal.ActionRestartBM] != Replace || entries["REPLACE_DISK"] != Replace || entries[signal.ActionRestartVM] != Restart {
		t.Errorf("Entries() does not reflect the effective table: %v", entries)
	}
	entries[signal.ActionReplaceVM] = Report
	if got, _ := table.Lookup(signal.ActionReplaceVM); got != Replace {
		t.Error("Entries() must return a copy; mutating it changed the table")
	}
}

func TestParseDecision(t *testing.T) {
	for name, want := range map[string]Decision{"Replace": Replace, "restart": Restart, "REPORT": Report} {
		got, err := ParseDecision(name)
		if err != nil || got != want {
			t.Errorf("ParseDecision(%q) = %s, %v; want %s, nil", name, got, err, want)
		}
	}
	if _, err := ParseDecision("Delete"); err == nil {
		t.Error("ParseDecision(\"Delete\") error = nil, want an error")
	}
}
