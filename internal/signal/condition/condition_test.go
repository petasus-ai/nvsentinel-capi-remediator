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

package condition

import (
	"reflect"
	"strings"
	"testing"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal"
)

// imexMessage is a verbatim node condition message observed on a GPU node
// running NVSentinel v1.20.0. DCGM reports the IMEX daemon as not ready on a
// topology that has no multi-node NVLink domain, which NVSentinel surfaces as
// GpuNvlinkWatch with CONTACT_SUPPORT. It is a known upstream false positive
// (NVIDIA/NVSentinel#1471) and the reason the recommended action, not the
// condition status, has to drive what happens to the node.
const imexMessage = "ErrorCode:DCGM_FR_IMEX_UNHEALTHY GPU:0 PCI:0000:ef:00.0 " +
	"GPU_UUID:GPU-b5f21324-f1f9-f1a4-1a37-1166234f6e4f IMEX daemon status is -3 (not READY) " +
	"Check IMEX installation, configuration, domain and daemon status, and network connectivity. " +
	"Recommended Action=CONTACT_SUPPORT;"

func TestParseIMEXFalsePositive(t *testing.T) {
	got, ok := Parse("GpuNvlinkWatch", imexMessage)
	if !ok {
		t.Fatal("expected the IMEX message to be recognised as an NVSentinel condition")
	}

	want := signal.Signal{
		Origin:     signal.OriginNodeCondition,
		Check:      "GpuNvlinkWatch",
		Actions:    []string{signal.ActionContactSupport},
		ErrorCodes: []string{"DCGM_FR_IMEX_UNHEALTHY"},
		GpuUUIDs:   []string{"GPU-b5f21324-f1f9-f1a4-1a37-1166234f6e4f"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Parse() = %+v, want %+v", got, want)
	}
}

// TestParseObservedFaults covers two faults injected on a real node. Both
// messages are verbatim and pin the two ends of what NVSentinel recommends: a
// fatal GPU fault that only asks for a restart, and a NIC fault that asks for
// a replacement.
func TestParseObservedFaults(t *testing.T) {
	tests := []struct {
		name          string
		conditionType string
		message       string
		wantAction    string
		wantErrorCode string
	}{
		{
			name:          "xid 79 gpu fell off the bus",
			conditionType: "SysLogsXIDError",
			message: "ErrorCode:79 PCI:0000:ef:00 GPU_UUID:GPU-b5f21324-f1f9-f1a4-1a37-1166234f6e4f " +
				"MESSAGE=NVRM: Xid (PCI:0000:ef:00): 79, pid=0, name=<unknown>, " +
				"GPU has fallen off the bus. Recommended Action=RESTART_BM;",
			wantAction:    signal.ActionRestartBM,
			wantErrorCode: "79",
		},
		{
			name:          "mlx5 unrecoverable hardware error",
			conditionType: "SysLogsNICDriverError",
			message: "ErrorCode:unrecoverable_err MESSAGE=mlx5_core 0000:c1:00.0: mlx5_crdump_collect: " +
				"unrecoverable hardware error detected Recommended Action=REPLACE_VM;",
			wantAction:    signal.ActionReplaceVM,
			wantErrorCode: "unrecoverable_err",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Parse(tt.conditionType, tt.message)
			if !ok {
				t.Fatal("expected the message to be recognised as an NVSentinel condition")
			}
			if got.Check != tt.conditionType {
				t.Errorf("Check = %q, want %q", got.Check, tt.conditionType)
			}
			if want := []string{tt.wantAction}; !reflect.DeepEqual(got.Actions, want) {
				t.Errorf("Actions = %v, want %v", got.Actions, want)
			}
			if want := []string{tt.wantErrorCode}; !reflect.DeepEqual(got.ErrorCodes, want) {
				t.Errorf("ErrorCodes = %v, want %v", got.ErrorCodes, want)
			}
			if got.Truncated {
				t.Error("nothing was cut from the message, want Truncated=false")
			}
		})
	}
}

func TestParseIgnoresKubeletConditions(t *testing.T) {
	if _, ok := Parse("Ready", "kubelet is posting ready status"); ok {
		t.Error("kubelet conditions carry no recommended action and must not be decoded")
	}
}

func TestParseIgnoresHealthySentinel(t *testing.T) {
	// The connector writes this when every event of a check has cleared.
	if _, ok := Parse("GpuXidWatch", "No Health Failures"); ok {
		t.Error("the recovery sentinel carries no action and must not be decoded")
	}
}

// Events the tests below assemble the way the connector does. Each is one
// health event with its trailing separator stripped, as the connector stores
// them before joining.
const (
	replaceEvent = "ErrorCode:DCGM_FR_XID_ERROR GPU:2 GPU_UUID:GPU-b5f21324-f1f9-f1a4-1a37-1166234f6e4f " +
		"xid 79 seen Recommended Action=REPLACE_VM"
	supportEvent = "ErrorCode:DCGM_FR_PCI_REPLAY_RATE GPU:2 PCI:0000:ef:00.0 " +
		"replay rate high Recommended Action=CONTACT_SUPPORT"
)

// join builds a condition message the way the connector's tier-2 truncation
// does: events joined by ';', a trailing ';', and when an event does not fit,
// as much of its prefix as the remaining space allows followed by the
// truncation marker. This is a port of truncateNodeConditionMessage
// (platform-connectors/pkg/connectors/kubernetes/process_node_events.go) so
// the tests exercise the real shape, partial event included.
func join(events []string, maxLen int) string {
	var (
		b         strings.Builder
		truncated bool
	)
	for i, ev := range events {
		sep := ""
		if i > 0 {
			sep = eventSeparator
		}
		if b.Len()+len(sep)+len(ev)+1 > maxLen {
			available := maxLen - b.Len() - len(sep) - 1 - len(truncationSuffix)
			if available > 0 {
				b.WriteString(sep)
				b.WriteString(ev[:available])
			}
			truncated = true

			break
		}
		b.WriteString(sep)
		b.WriteString(ev)
	}
	b.WriteString(eventSeparator)
	if truncated {
		b.WriteString(truncationSuffix)
	}

	return b.String()
}

func TestParseMultipleEvents(t *testing.T) {
	msg := join([]string{replaceEvent, supportEvent}, 1024)

	got, ok := Parse("GpuPcieWatch", msg)
	if !ok {
		t.Fatal("expected the message to be recognised")
	}
	if got.Truncated {
		t.Error("both events fit, want Truncated=false")
	}
	if want := []string{signal.ActionReplaceVM, signal.ActionContactSupport}; !reflect.DeepEqual(got.Actions, want) {
		t.Errorf("Actions = %v, want %v", got.Actions, want)
	}
	if want := []string{"DCGM_FR_XID_ERROR", "DCGM_FR_PCI_REPLAY_RATE"}; !reflect.DeepEqual(got.ErrorCodes, want) {
		t.Errorf("ErrorCodes = %v, want %v", got.ErrorCodes, want)
	}
}

func TestParseDeduplicatesSharedValues(t *testing.T) {
	// Two GPUs of one node fail the same check with the same code and the
	// same action. The connector stores each event once, but the values they
	// share must not be reported twice.
	secondGPU := "ErrorCode:DCGM_FR_XID_ERROR GPU:3 GPU_UUID:GPU-0f6c2a1e-1c2d-4b3a-9e8f-7a6b5c4d3e2f " +
		"xid 79 seen Recommended Action=REPLACE_VM"
	msg := join([]string{replaceEvent, secondGPU}, 1024)

	got, ok := Parse("GpuXidWatch", msg)
	if !ok {
		t.Fatal("expected the message to be recognised")
	}
	if want := []string{signal.ActionReplaceVM}; !reflect.DeepEqual(got.Actions, want) {
		t.Errorf("Actions = %v, want %v", got.Actions, want)
	}
	if want := []string{"DCGM_FR_XID_ERROR"}; !reflect.DeepEqual(got.ErrorCodes, want) {
		t.Errorf("ErrorCodes = %v, want %v", got.ErrorCodes, want)
	}
	if want := []string{
		"GPU-b5f21324-f1f9-f1a4-1a37-1166234f6e4f",
		"GPU-0f6c2a1e-1c2d-4b3a-9e8f-7a6b5c4d3e2f",
	}; !reflect.DeepEqual(got.GpuUUIDs, want) {
		t.Errorf("GpuUUIDs = %v, want %v (distinct devices are all reported)", got.GpuUUIDs, want)
	}
}

func TestParseCompactedEventIsNotTruncation(t *testing.T) {
	// Tier-1 compaction shortens the free text inside an event and marks it
	// with "..." followed by the action. No event was lost.
	compacted := "ErrorCode:DCGM_FR_XID_ERROR GPU:2 GPU_UUID:GPU-b5f21324-f1f9-f1a4-1a37-1166234f6e4f " +
		"xid 79 se... Recommended Action=REPLACE_VM"
	msg := join([]string{compacted, supportEvent}, 1024)

	got, ok := Parse("GpuXidWatch", msg)
	if !ok {
		t.Fatal("expected the message to be recognised")
	}
	if got.Truncated {
		t.Error("compaction inside an event must not be reported as truncation")
	}
	if want := []string{signal.ActionReplaceVM, signal.ActionContactSupport}; !reflect.DeepEqual(got.Actions, want) {
		t.Errorf("Actions = %v, want %v", got.Actions, want)
	}
	if want := []string{"GPU-b5f21324-f1f9-f1a4-1a37-1166234f6e4f"}; !reflect.DeepEqual(got.GpuUUIDs, want) {
		t.Errorf("GpuUUIDs = %v, want %v", got.GpuUUIDs, want)
	}
}

func TestParseTruncatedMessages(t *testing.T) {
	// Space needed to keep supportEvent whole plus the separators around it.
	whole := len(supportEvent) + len(eventSeparator) + 1

	tests := []struct {
		name string
		// maxLen is the connector's limit; the second event is cut at
		// maxLen minus what the first event, separators and marker use.
		maxLen         int
		wantActions    []string
		wantErrorCodes []string
		wantUUIDs      []string
	}{
		{
			// Nothing of the second event fits, not even a prefix.
			name:           "second event dropped whole",
			maxLen:         len(supportEvent) + 2,
			wantActions:    []string{signal.ActionContactSupport},
			wantErrorCodes: []string{"DCGM_FR_PCI_REPLAY_RATE"},
		},
		{
			// Cut inside the GPU UUID: "GPU_UUID:GPU-b5f2" is what remains.
			name:           "second event cut inside the GPU UUID",
			maxLen:         whole + len(truncationSuffix) + strings.Index(replaceEvent, "GPU-b5f21324") + len("GPU-b5f2"),
			wantActions:    []string{signal.ActionContactSupport},
			wantErrorCodes: []string{"DCGM_FR_PCI_REPLAY_RATE"},
		},
		{
			// Cut inside the action name: "Recommended Action=REPL" remains,
			// which must not surface as an action.
			name:           "second event cut inside the action name",
			maxLen:         whole + len(truncationSuffix) + strings.Index(replaceEvent, actionMarker) + len(actionMarker) + len("REPL"),
			wantActions:    []string{signal.ActionContactSupport},
			wantErrorCodes: []string{"DCGM_FR_PCI_REPLAY_RATE"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := join([]string{supportEvent, replaceEvent}, tt.maxLen)
			if !strings.HasSuffix(msg, eventSeparator+truncationSuffix) {
				t.Fatalf("test setup: expected a truncated message, got %q", msg)
			}

			got, ok := Parse("GpuXidWatch", msg)
			if !ok {
				t.Fatal("expected the message to be recognised")
			}
			if !got.Truncated {
				t.Error("a message ending in the connector's marker must be flagged as truncated")
			}
			if !reflect.DeepEqual(got.Actions, tt.wantActions) {
				t.Errorf("Actions = %v, want %v", got.Actions, tt.wantActions)
			}
			if !reflect.DeepEqual(got.ErrorCodes, tt.wantErrorCodes) {
				t.Errorf("ErrorCodes = %v, want %v", got.ErrorCodes, tt.wantErrorCodes)
			}
			if !reflect.DeepEqual(got.GpuUUIDs, tt.wantUUIDs) {
				t.Errorf("GpuUUIDs = %v, want %v (no partial identifier may leak)", got.GpuUUIDs, tt.wantUUIDs)
			}
		})
	}
}

func TestParseTruncationIsNotLengthBased(t *testing.T) {
	// With the default 1024-byte limit, a kept event of 1019 bytes leaves no
	// room for a prefix of the next one, and the result is 1023 bytes: under
	// the limit, yet an event was dropped. Only the marker tells.
	padding := strings.Repeat("x", 1019-len("ErrorCode:E1 GPU:0  Recommended Action=CONTACT_SUPPORT"))
	kept := "ErrorCode:E1 GPU:0 " + padding + " Recommended Action=CONTACT_SUPPORT"
	if len(kept) != 1019 {
		t.Fatalf("test setup: kept event is %d bytes, want 1019", len(kept))
	}
	msg := join([]string{kept, replaceEvent}, 1024)
	if len(msg) != 1023 {
		t.Fatalf("test setup: message is %d bytes, want 1023", len(msg))
	}

	got, ok := Parse("GpuXidWatch", msg)
	if !ok {
		t.Fatal("expected the message to be recognised")
	}
	if !got.Truncated {
		t.Error("an event was dropped although the message is under the limit; want Truncated=true")
	}
	if want := []string{signal.ActionContactSupport}; !reflect.DeepEqual(got.Actions, want) {
		t.Errorf("Actions = %v, want %v", got.Actions, want)
	}

	// Conversely, a long message with nothing cut is not truncated.
	long := join([]string{kept}, 4096)
	if got, _ := Parse("GpuXidWatch", long); got.Truncated {
		t.Error("a long message that was not cut must not be flagged as truncated")
	}
}
