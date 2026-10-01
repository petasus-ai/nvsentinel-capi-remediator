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
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal/condition"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal/extrr"
)

// Mode is how the signals of one workload cluster are read and acted on,
// chosen from what NVSentinel installed there.
type Mode string

const (
	// ModeExternalRemediationRequest reads ExternalRemediationRequests and
	// nothing else. The resource ships with NVSentinel's janitor, since
	// NVSentinel v1.10, and the janitor remediates by itself every action
	// that is not routed to a request, so node conditions are left to it.
	// Before v1.13 the janitor never releases a node to a request, so such
	// a cluster is observed without anything to report.
	ModeExternalRemediationRequest Mode = "ExternalRemediationRequest"
	// ModeReportOnly reads node conditions and only reports them. The
	// janitor is installed without ExternalRemediationRequest, as before
	// NVSentinel v1.10, so it remediates the faults itself and acting on
	// them too would remediate the same node twice.
	ModeReportOnly Mode = "ReportOnly"
	// ModeNodeCondition reads node conditions and acts on them. NVSentinel
	// only detects faults in this cluster.
	ModeNodeCondition Mode = "NodeCondition"
)

// janitorGroupVersionKind is one of the resources NVSentinel's janitor
// installs. They are installed together, so one stands for all of them.
// Every release that has a janitor serves them in v1alpha1.
var janitorGroupVersionKind = schema.GroupVersionKind{Group: "janitor.dgxc.nvidia.com", Version: "v1alpha1", Kind: "RebootNode"}

// selectMode chooses the mode of a workload cluster from the resources it
// serves. A resource that is not served is a no-match from the mapper,
// which asks the API server for that group version again on every lookup,
// so a resource installed later is picked up by the next poll; that costs
// one discovery request per poll and resource not served. A resource the
// mapper has found stays found: removing a CRD is only noticed once the
// cluster cache reconnects, and CRDs that Helm leaves behind when the
// janitor is disabled keep the cluster in the mode they imply. Any other
// error is returned.
func selectMode(mapper meta.RESTMapper) (Mode, error) {
	gvk := extrr.GroupVersionKind
	served, err := isServed(mapper, gvk.GroupKind(), gvk.Version)
	switch {
	case err != nil:
		return "", err
	case served:
		return ModeExternalRemediationRequest, nil
	}

	served, err = isServed(mapper, janitorGroupVersionKind.GroupKind(), janitorGroupVersionKind.Version)
	switch {
	case err != nil:
		return "", err
	case served:
		return ModeReportOnly, nil
	default:
		return ModeNodeCondition, nil
	}
}

// isServed reports whether the cluster serves the kind in the version.
func isServed(mapper meta.RESTMapper, gk schema.GroupKind, version string) (bool, error) {
	_, err := mapper.RESTMapping(gk, version)
	switch {
	case err == nil:
		return true, nil
	case meta.IsNoMatchError(err):
		return false, nil
	default:
		return false, fmt.Errorf("look up %s: %w", gk, err)
	}
}

// source returns the signal source the mode reads from.
func (m Mode) source(reader client.Reader) signal.Source {
	if m == ModeExternalRemediationRequest {
		return extrr.NewSource(reader)
	}

	return condition.NewSource(reader)
}

// describe explains the mode in a sentence, for logs and Events.
func (m Mode) describe() string {
	switch m {
	case ModeExternalRemediationRequest:
		return "reading NVSentinel's ExternalRemediationRequests; faults not routed to one are left to NVSentinel's janitor"
	case ModeReportOnly:
		return "reading NVSentinel's node conditions and only reporting them, since NVSentinel's janitor remediates this cluster itself"
	default:
		return "reading NVSentinel's node conditions"
	}
}
