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

package main

import (
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
)

func TestParseDrivers(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
		bad  bool
	}{
		{in: "gpu.nvidia.com", want: []string{"gpu.nvidia.com"}},
		{in: " nic.example.com, gpu.nvidia.com ,gpu.nvidia.com,", want: []string{"gpu.nvidia.com", "nic.example.com"}},
		{in: "", bad: true},
		{in: " , ", bad: true},
		{in: "GPU.nvidia.com", bad: true},
		{in: "gpu.nvidia.com/x", bad: true},
	} {
		got, err := parseDrivers(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("parseDrivers(%q) = %v; want an error", c.in, sets.List(got))
			}
			continue
		}
		if err != nil || !slices.Equal(sets.List(got), c.want) {
			t.Errorf("parseDrivers(%q) = %v, %v; want %v", c.in, sets.List(got), err, c.want)
		}
	}
}
