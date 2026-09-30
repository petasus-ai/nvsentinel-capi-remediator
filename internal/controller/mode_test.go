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
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal/condition"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal/extrr"
)

// newMapper returns a mapper serving the given kinds, all cluster-scoped
// like NVSentinel's. Their versions are the preferred ones, as discovery
// reports them, so that a lookup without a version finds them.
func newMapper(gvks ...schema.GroupVersionKind) *meta.DefaultRESTMapper {
	versions := make([]schema.GroupVersion, 0, len(gvks))
	for _, gvk := range gvks {
		versions = append(versions, gvk.GroupVersion())
	}

	mapper := meta.NewDefaultRESTMapper(versions)
	for _, gvk := range gvks {
		mapper.Add(gvk, meta.RESTScopeRoot)
	}

	return mapper
}

// failingMapper fails every lookup of one group for a reason other than a
// missing kind, as discovery does while an API server is unavailable.
type failingMapper struct {
	meta.RESTMapper
	group string
	err   error
}

func (m failingMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	if gk.Group == m.group {
		return nil, m.err
	}

	return m.RESTMapper.RESTMapping(gk, versions...)
}

func TestSelectMode(t *testing.T) {
	tests := []struct {
		name   string
		mapper meta.RESTMapper
		want   Mode
	}{
		{"janitor with requests", newMapper(extrr.GroupVersionKind, janitorGroupVersionKind), ModeExternalRemediationRequest},
		{"requests alone", newMapper(extrr.GroupVersionKind), ModeExternalRemediationRequest},
		{"janitor before requests existed", newMapper(janitorGroupVersionKind), ModeReportOnly},
		// A version this operator cannot read counts as not served.
		{"requests in another version", newMapper(extrr.GroupVersionKind.GroupKind().WithVersion("v2"), janitorGroupVersionKind), ModeReportOnly},
		{"detection only", newMapper(), ModeNodeCondition},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectMode(tt.mapper)
			if err != nil || got != tt.want {
				t.Fatalf("selectMode() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestSelectModeTreatsAMissingGroupVersionAsNotServed(t *testing.T) {
	// The dynamic mapper's first, aggregated discovery reports a group
	// version it cannot find this way; the error unwraps to a no-match.
	gv := extrr.GroupVersionKind.GroupVersion()
	missing := &apiutil.ErrResourceDiscoveryFailed{gv: &meta.NoResourceMatchError{PartialResource: gv.WithResource("")}}
	mapper := failingMapper{RESTMapper: newMapper(), group: gv.Group, err: missing}

	if got, err := selectMode(mapper); err != nil || got != ModeNodeCondition {
		t.Fatalf("selectMode() = %q, %v; want %q", got, err, ModeNodeCondition)
	}
}

func TestSelectModeReportsLookupFailures(t *testing.T) {
	boom := errors.New("discovery unavailable")
	for _, group := range []string{extrr.GroupVersionKind.Group, janitorGroupVersionKind.Group} {
		mapper := failingMapper{RESTMapper: newMapper(), group: group, err: boom}
		if _, err := selectMode(mapper); !errors.Is(err, boom) {
			t.Errorf("failing %s: selectMode() error = %v, want it to wrap %v", group, err, boom)
		}
	}
}

func TestModeSource(t *testing.T) {
	tests := map[Mode]string{
		ModeExternalRemediationRequest: extrr.SourceName,
		ModeReportOnly:                 condition.SourceName,
		ModeNodeCondition:              condition.SourceName,
	}
	for mode, want := range tests {
		if got := mode.source(nil).Name(); got != want {
			t.Errorf("%s reads from %q, want %q", mode, got, want)
		}
		if mode.describe() == "" {
			t.Errorf("%s has no description", mode)
		}
	}
}
