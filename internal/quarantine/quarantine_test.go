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

package quarantine

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	gpuDriver = "gpu.example.com"
	nicDriver = "nic.example.com"
)

var (
	gpu0 = Device{Driver: gpuDriver, Pool: "node-a", Name: "gpu-0"}
	gpu1 = Device{Driver: gpuDriver, Pool: "node-a", Name: "gpu-1"}
	nic0 = Device{Driver: nicDriver, Pool: "node-a", Name: "nic-0"}
)

// reasonOf returns the reason of a Rejection, or "" for any other error.
func reasonOf(err error) Reason {
	var r *Rejection
	if errors.As(err, &r) {
		return r.Reason
	}

	return ""
}

func TestParseRequest(t *testing.T) {
	cases := []struct {
		value  string
		want   []string
		reject bool
	}{
		{value: "quarantine"},
		{value: "quarantine:gpu-0", want: []string{"gpu-0"}},
		{value: "quarantine:gpu-1, gpu-0,gpu-1", want: []string{"gpu-1", "gpu-0"}},
		{value: "quarantine:", reject: true},
		{value: "quarantine:gpu-0,", reject: true},
		{value: "quarantine-gpu", reject: true},
		{value: "Quarantine", reject: true},
		{value: "", reject: true},
	}
	for _, c := range cases {
		got, err := ParseRequest(c.value)
		if c.reject {
			if reasonOf(err) != ReasonInvalidRequest {
				t.Errorf("ParseRequest(%q) = %v, %v; want an InvalidRequest rejection", c.value, got, err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("ParseRequest(%q) = %v, %v; want %v", c.value, got, err, c.want)
		}
	}
}

func TestSelect(t *testing.T) {
	gpus := sets.New(gpuDriver)
	otherPool := Device{Driver: gpuDriver, Pool: "node-b", Name: "gpu-0"}

	cases := []struct {
		name      string
		all       []Device
		drivers   sets.Set[string]
		requested []string
		want      []Device
		reason    Reason
		message   string
	}{
		{name: "the one device of the drivers", all: []Device{nic0, gpu0}, drivers: gpus, want: []Device{gpu0}},
		{name: "a repeated device counts once", all: []Device{gpu0, gpu0}, drivers: gpus, want: []Device{gpu0}},
		{name: "several drivers", all: []Device{nic0, gpu0}, drivers: sets.New(gpuDriver, nicDriver), reason: ReasonAmbiguousDevices},
		{name: "none of the drivers", all: []Device{nic0}, drivers: gpus, reason: ReasonNoDevices, message: nic0.String()},
		{name: "nothing at all", drivers: gpus, reason: ReasonNoDevices},
		{name: "several without a list", all: []Device{gpu1, gpu0}, drivers: gpus, reason: ReasonAmbiguousDevices, message: "gpu-0, gpu-1"},
		{name: "listed", all: []Device{gpu1, gpu0, nic0}, drivers: gpus, requested: []string{"gpu-1"}, want: []Device{gpu1}},
		{name: "listed in any order", all: []Device{gpu0, gpu1}, drivers: gpus, requested: []string{"gpu-1", "gpu-0"}, want: []Device{gpu0, gpu1}},
		{name: "listed one of a single device", all: []Device{gpu0}, drivers: gpus, requested: []string{"gpu-0"}, want: []Device{gpu0}},
		{name: "listed but unknown", all: []Device{gpu0, gpu1}, drivers: gpus, requested: []string{"gpu-7"}, reason: ReasonUnknownDevice},
		{name: "listed but of another driver", all: []Device{gpu0, nic0}, drivers: gpus, requested: []string{"nic-0"}, reason: ReasonUnknownDevice},
		{name: "a name in two pools", all: []Device{gpu0, otherPool}, drivers: gpus, requested: []string{"gpu-0"}, reason: ReasonAmbiguousDevices, message: "node-b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Select(c.all, c.drivers, c.requested)
			if c.reason != "" {
				if reasonOf(err) != c.reason {
					t.Fatalf("Select = %v, %v; want a %s rejection", got, err, c.reason)
				}
				if !strings.Contains(err.Error(), c.message) {
					t.Errorf("rejection %q does not mention %q", err, c.message)
				}
				return
			}
			if err != nil || !slices.Equal(got, c.want) {
				t.Errorf("Select = %v, %v; want %v", got, err, c.want)
			}
		})
	}
}

func TestRuleName(t *testing.T) {
	name := RuleName(gpu0)
	if name != RuleName(Device{Driver: gpuDriver, Pool: "node-a", Name: "gpu-0"}) {
		t.Error("the rule name of a device changes")
	}
	if !strings.HasPrefix(name, "quarantine-") || len(name) != len("quarantine-")+16 {
		t.Errorf("RuleName = %q; want quarantine- and 16 hex digits", name)
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		t.Errorf("RuleName = %q is not an object name: %v", name, errs)
	}
	seen := map[string]Device{}
	for _, d := range []Device{gpu0, gpu1, nic0, {Driver: gpuDriver, Pool: "node-a/gpu", Name: "0"}, {Driver: gpuDriver, Pool: "node-a", Name: "gpu/0"}} {
		if other, ok := seen[RuleName(d)]; ok {
			t.Errorf("%s and %s share the rule name %s", d, other, RuleName(d))
		}
		seen[RuleName(d)] = d
	}
}

func TestLabelValue(t *testing.T) {
	short := "gpu-cluster-gpu-np-vdj6w-vt92l"
	if got := labelValue(short); got != short {
		t.Errorf("labelValue(%q) = %q; want it unchanged", short, got)
	}

	long := strings.Repeat("a", 60) + ".b-" + strings.Repeat("c", 30)
	got := labelValue(long)
	if errs := validation.IsValidLabelValue(got); len(errs) > 0 {
		t.Errorf("labelValue(%q) = %q is not a label value: %v", long, got, errs)
	}
	if !strings.HasPrefix(got, long[:40]) {
		t.Errorf("labelValue(%q) = %q does not keep the start of the name", long, got)
	}
	if other := labelValue(long + "d"); other == got {
		t.Errorf("two long names share the label value %q", got)
	}
}

func TestStatus(t *testing.T) {
	s := RejectedStatus(ReasonNoDevices)
	if s != "rejected: NoDevices" || !IsRejected(s) {
		t.Errorf("RejectedStatus = %q, IsRejected = %v", s, IsRejected(s))
	}
	for _, s := range []string{"", StatusQuarantining, StatusReplacing} {
		if IsRejected(s) {
			t.Errorf("IsRejected(%q) = true", s)
		}
	}
}
