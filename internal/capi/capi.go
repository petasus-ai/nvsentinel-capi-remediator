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

// Package capi carries out remediation decisions on Cluster API Machines.
//
// It relies only on the core Cluster API contract, so it works with every
// infrastructure provider. A Machine is handed over by stamping the
// remediate-machine annotation: the MachineHealthCheck covering it then
// treats the Machine as unhealthy regardless of its own checks and applies
// its configured remediation. Without a remediation template that is
// replacement by the owning MachineSet; with one it is whatever the
// provider's template does. A Machine that no MachineHealthCheck covers is
// not acted on at all. Resolving which MachineHealthCheck covers a Machine,
// and so what the annotation will do, is the caller's job.
package capi

import (
	"unicode/utf8"

	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// AnnotationPrefix scopes every annotation this operator writes. It is
	// the one place to change should the project move to another owner.
	AnnotationPrefix = "nvsentinel.petasus.io"

	// RemediationReasonAnnotation records why this operator asked Cluster API
	// to remediate an object. Cluster API's own remediate-machine annotation
	// is presence-only, so without this an operator reading the Machine could
	// not tell a GPU fault from any other trigger.
	RemediationReasonAnnotation = AnnotationPrefix + "/remediation-reason"
)

// Event reasons recorded on Machines. Annotations die with the Machine and
// remediation may delete it, so an Event is the only record of a fault that
// outlives the node it happened on. Every Event also names the action it
// is about, which the events API keeps as a separate field.
const (
	// ActionRemediate is the action of Events about handing a Machine to
	// Cluster API for remediation, whether that happened or was skipped.
	ActionRemediate = "Remediate"

	// EventMarkedForRemediation is recorded once the remediate-machine
	// annotation is in place.
	EventMarkedForRemediation = "MarkedForRemediation"
	// EventRemediationSkipped is recorded when a signal called for
	// remediation but a guard left the Machine untouched and nothing else
	// is going to act on it.
	EventRemediationSkipped = "RemediationSkipped"
)

// EventNoteLimit is the longest note the events API accepts, in bytes. A
// longer note fails validation and the Event is lost.
const EventNoteLimit = 1024

// EventNote fits a note into what the events API accepts, marking the cut.
func EventNote(note string) string {
	if len(note) <= EventNoteLimit {
		return note
	}

	const marker = "..."
	cut := EventNoteLimit - len(marker)
	for cut > 0 && !utf8.RuneStart(note[cut]) {
		cut--
	}

	return note[:cut] + marker
}

// Actuator applies decisions to Machines in the management cluster.
type Actuator struct {
	// Client reaches the management cluster.
	Client client.Client
	// Recorder records an Event on the Machine for every action taken. It
	// is required unless DryRun is set, which never records.
	Recorder events.EventRecorder
	// DryRun evaluates the guards but writes nothing: no annotation and no
	// Event. The results then describe what would have happened.
	DryRun bool
}
