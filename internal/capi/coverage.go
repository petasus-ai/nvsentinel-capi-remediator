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

package capi

import (
	"context"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Coverage tells what Cluster API does with a Machine once it carries the
// remediate-machine annotation. Every MachineHealthCheck that selects the
// Machine acts on the annotation, each with its own remediation: one
// without a template has the owner replace the Machine, one with a
// template creates a request from it for the provider to carry out.
//
// Requests are left to the MachineHealthChecks on purpose. A check deletes
// the request named after every Machine it finds healthy, and the signals
// this operator acts on are not among its checks, so a request created
// behind its back would be deleted on the check's next reconcile. Marking
// the Machine instead keeps the request's lifecycle and the check's
// unhealthy-count gates with Cluster API.
type Coverage struct {
	// Checks are the MachineHealthChecks that select the Machine, sorted
	// by name.
	Checks []CoveringCheck
}

// CoveringCheck is one MachineHealthCheck that selects a Machine.
type CoveringCheck struct {
	Name string
	// Template is the check's remediation template, nil when the check
	// remediates by replacement.
	Template *clusterv1.MachineHealthCheckRemediationTemplateReference
	// Paused is set when the check carries the paused annotation. Cluster
	// API does not reconcile a paused check, so it acts on the annotation
	// only at some arbitrary time after it is unpaused.
	Paused bool
}

// Covered reports whether a MachineHealthCheck that is not paused selects
// the Machine. Without one the remediate-machine annotation does nothing,
// or nothing until a check is unpaused.
func (c Coverage) Covered() bool {
	for _, check := range c.Checks {
		if !check.Paused {
			return true
		}
	}

	return false
}

// TemplatesOnly reports whether the Machine is covered and every check
// selecting it, paused or not, remediates through a template, so that
// marking the Machine hands it to the provider's remediation and to no
// replacement. A single check without a template would have the owner
// replace the Machine at the same time, or once that check is unpaused.
//
// The template's kind is the provider's to define. This operator trusts
// that a check configured with one restarts the Machine, which is what the
// providers offering remediation templates implement, and that the
// provider falls back to replacement once its retries are exhausted. Two
// checks with templates each create their request, so the provider may
// run two remediations at once; Cluster API leaves avoiding that overlap
// to whoever configures the checks.
func (c Coverage) TemplatesOnly() bool {
	if !c.Covered() {
		return false
	}

	for _, check := range c.Checks {
		if check.Template == nil {
			return false
		}
	}

	return true
}

// IgnoringPause returns the coverage as it will be once every paused check
// is unpaused, which tells whether a decision the checks cannot carry out
// now will become possible or never will.
func (c Coverage) IgnoringPause() Coverage {
	unpaused := Coverage{Checks: make([]CoveringCheck, len(c.Checks))}
	for i, check := range c.Checks {
		check.Paused = false
		unpaused.Checks[i] = check
	}

	return unpaused
}

// Describe explains the coverage in a clause, for logs and Events, e.g.
// "MachineHealthCheck gpu-np remediates through RebootRemediationTemplate
// reboot".
func (c Coverage) Describe() string {
	if len(c.Checks) == 0 {
		return "no MachineHealthCheck selects the Machine"
	}

	parts := make([]string, 0, len(c.Checks))
	for _, check := range c.Checks {
		how := "remediates by replacement"
		if check.Template != nil {
			how = "remediates through " + check.Template.Kind + " " + check.Template.Name
		}
		if check.Paused {
			how += " (paused)"
		}
		parts = append(parts, "MachineHealthCheck "+check.Name+" "+how)
	}

	return strings.Join(parts, "; ")
}

// CoverageOf resolves which of the given MachineHealthChecks select the
// Machine, the same way Cluster API does: a check in the Machine's
// namespace, for its Cluster, whose selector matches the Machine's labels
// with the Cluster's name added. A check whose selector does not parse
// selects nothing, as Cluster API then fails that check's reconcile.
func CoverageOf(checks []clusterv1.MachineHealthCheck, m *clusterv1.Machine) Coverage {
	var cov Coverage
	for i := range checks {
		mhc := &checks[i]
		if mhc.Namespace != m.Namespace || mhc.Spec.ClusterName != m.Spec.ClusterName {
			continue
		}

		selector, err := metav1.LabelSelectorAsSelector(metav1.CloneSelectorAndAddLabel(
			&mhc.Spec.Selector, clusterv1.ClusterNameLabel, mhc.Spec.ClusterName,
		))
		if err != nil || !selector.Matches(labels.Set(m.Labels)) {
			continue
		}

		check := CoveringCheck{Name: mhc.Name, Paused: annotations.HasPaused(mhc)}
		if mhc.Spec.Remediation.TemplateRef.IsDefined() {
			ref := mhc.Spec.Remediation.TemplateRef
			check.Template = &ref
		}
		cov.Checks = append(cov.Checks, check)
	}

	slices.SortFunc(cov.Checks, func(a, b CoveringCheck) int {
		return strings.Compare(a.Name, b.Name)
	})

	return cov
}

// ListHealthChecks lists the MachineHealthChecks of a Cluster: every check
// that can cover one of its Machines.
func ListHealthChecks(ctx context.Context, c client.Reader, cluster *clusterv1.Cluster) ([]clusterv1.MachineHealthCheck, error) {
	list := &clusterv1.MachineHealthCheckList{}
	if err := c.List(ctx, list, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, fmt.Errorf("list machinehealthchecks of %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}

	checks := make([]clusterv1.MachineHealthCheck, 0, len(list.Items))
	for _, mhc := range list.Items {
		if mhc.Spec.ClusterName == cluster.Name {
			checks = append(checks, mhc)
		}
	}

	return checks, nil
}
