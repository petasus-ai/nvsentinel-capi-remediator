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

// Package quarantine takes the DRA devices of a Machine's workload out of
// scheduling, so that the Machine can be replaced without its replacement
// being given the same faulty device again.
//
// The devices are tainted with DeviceTaintRules, the standard way for an
// administrator to taint a device: the scheduler honours a rule whatever
// driver publishes the device, and the driver needs no part in it. Finding
// the devices depends on how the infrastructure provider runs the workload.
// Locator stands for that, and KubeVirtLocator implements it for Cluster API
// Provider KubeVirt, whose VMs are handed their devices by the management
// cluster's own DRA drivers.
package quarantine

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/capi"
)

const (
	// ReplaceAnnotation on a Machine is an operator's request to replace it
	// once the devices its workload holds are quarantined. Its value is
	// RequestQuarantine, alone when the Machine holds one device of the
	// configured drivers, or followed by a colon and the comma-separated
	// names of the devices to quarantine, as their ResourceSlice names them.
	ReplaceAnnotation = capi.AnnotationPrefix + "/replace"
	// ReplaceStatusAnnotation records how far a request has got:
	// StatusQuarantining, StatusReplacing, or the reason it was rejected
	// (see RejectedStatus). A rejected request is not looked at again until
	// the annotation is removed.
	ReplaceStatusAnnotation = capi.AnnotationPrefix + "/replace-status"

	// RequestQuarantine is the value of ReplaceAnnotation, or its prefix
	// when devices are listed.
	RequestQuarantine = "quarantine"

	// StatusQuarantining means the rules are being created.
	StatusQuarantining = "quarantining"
	// StatusReplacing means every rule is in place and the Machine is
	// being deleted for its MachineSet to replace.
	StatusReplacing = "replacing"
	// rejectedPrefix starts the status of a rejected request.
	rejectedPrefix = "rejected: "

	// TaintKey is the key of the taint every rule puts on its device.
	TaintKey = capi.AnnotationPrefix + "/quarantined"

	// ManagedByLabel and ManagedBy mark the rules this operator created.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedBy      = "nvsentinel-capi-remediator"
	// MachineLabel, MachineNamespaceLabel and ClusterLabel on a rule name
	// the Machine it was created for and that Machine's Cluster, so that
	// the rules can be found by them. A name too long for a label value is
	// shortened (see labelValue); MachineAnnotation always has it whole.
	MachineLabel          = capi.AnnotationPrefix + "/machine"
	MachineNamespaceLabel = capi.AnnotationPrefix + "/machine-namespace"
	ClusterLabel          = capi.AnnotationPrefix + "/cluster"
	// MachineAnnotation, DeviceAnnotation and RequestedAtAnnotation on a
	// rule record, as namespace/name, driver/pool/device and an RFC 3339
	// time, what it was created for and when.
	MachineAnnotation     = capi.AnnotationPrefix + "/machine"
	DeviceAnnotation      = capi.AnnotationPrefix + "/device"
	RequestedAtAnnotation = capi.AnnotationPrefix + "/requested-at"

	// rulePrefix starts the name of every rule.
	rulePrefix = "quarantine-"
)

// RejectedStatus is the status of a request rejected for reason.
func RejectedStatus(reason Reason) string {
	return rejectedPrefix + string(reason)
}

// IsRejected reports whether a status is that of a rejected request.
func IsRejected(status string) bool {
	return strings.HasPrefix(status, rejectedPrefix)
}

// Reason names why a request cannot be carried out as it stands. Trying
// again changes nothing until someone changes the request or what it found.
type Reason string

const (
	// ReasonInvalidRequest the annotation's value is not a request.
	ReasonInvalidRequest Reason = "InvalidRequest"
	// ReasonNotKubeVirt the Machine is not a KubeVirt one, or KubeVirt is
	// not installed, so there is no way to find its devices.
	ReasonNotKubeVirt Reason = "NotKubeVirt"
	// ReasonExternalInfraCluster the Machine's VM runs in another cluster,
	// whose devices this operator cannot reach.
	ReasonExternalInfraCluster Reason = "ExternalInfraCluster"
	// ReasonNoDevices the Machine holds no device of the configured
	// drivers, or its VM is not running.
	ReasonNoDevices Reason = "NoDevices"
	// ReasonAmbiguousDevices the Machine holds more than one device of the
	// configured drivers and the request lists none, or a listed name fits
	// more than one.
	ReasonAmbiguousDevices Reason = "AmbiguousDevices"
	// ReasonUnknownDevice a listed name is not among the devices of the
	// configured drivers that the Machine holds.
	ReasonUnknownDevice Reason = "UnknownDevice"
	// ReasonDeviceTaintRuleUnavailable the cluster serves no version of
	// DeviceTaintRule this operator knows.
	ReasonDeviceTaintRuleUnavailable Reason = "DeviceTaintRuleUnavailable"
	// ReasonDeviceTaintRuleConflict a rule by the name this operator gives
	// a device's rule exists, but does not keep that device from being
	// scheduled.
	ReasonDeviceTaintRuleConflict Reason = "DeviceTaintRuleConflict"
	// ReasonForbidden this operator is not allowed to read or write what
	// the request needs.
	ReasonForbidden Reason = "Forbidden"
)

// Rejection is the error for a request that cannot be carried out as it
// stands.
type Rejection struct {
	Reason  Reason
	Message string
}

func (r *Rejection) Error() string {
	return string(r.Reason) + ": " + r.Message
}

// reject returns a Rejection for reason with a formatted message.
func reject(reason Reason, format string, args ...any) error {
	return &Rejection{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// Device is one allocated DRA device, named the way a DeviceTaintRule
// selects it.
type Device struct {
	Driver string
	Pool   string
	Name   string
}

// String names the device as driver/pool/device.
func (d Device) String() string {
	return d.Driver + "/" + d.Pool + "/" + d.Name
}

// compare orders devices by driver, pool and name.
func compare(a, b Device) int {
	return cmp.Or(cmp.Compare(a.Driver, b.Driver), cmp.Compare(a.Pool, b.Pool), cmp.Compare(a.Name, b.Name))
}

// ParseRequest reads the value of ReplaceAnnotation. It returns the device
// names the request lists, in order and without repeats, or none when it
// lists none.
func ParseRequest(value string) ([]string, error) {
	if value == RequestQuarantine {
		return nil, nil
	}

	list, ok := strings.CutPrefix(value, RequestQuarantine+":")
	if !ok {
		return nil, reject(ReasonInvalidRequest, "%q is neither %q nor %q followed by device names",
			value, RequestQuarantine, RequestQuarantine+":")
	}

	var names []string
	for name := range strings.SplitSeq(list, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, reject(ReasonInvalidRequest, "%q lists an empty device name", value)
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}

	return names, nil
}

// Select picks the devices a request quarantines out of all those the
// Machine holds: those of the given drivers, all of them when the request
// lists no names and there is just one, or else the ones named. A name is
// a device's name without its pool, which is unique among the devices of
// one VM in practice; one that fits more than one device is rejected rather
// than guessed at. The result is sorted and has no repeats.
func Select(all []Device, drivers sets.Set[string], requested []string) ([]Device, error) {
	var candidates []Device
	for _, d := range all {
		if drivers.Has(d.Driver) && !slices.Contains(candidates, d) {
			candidates = append(candidates, d)
		}
	}
	slices.SortFunc(candidates, compare)

	if len(requested) == 0 {
		switch len(candidates) {
		case 0:
			return nil, reject(ReasonNoDevices, "the Machine holds no device of %s%s",
				strings.Join(sets.List(drivers), ", "), heldNote(all))
		case 1:
			return candidates, nil
		default:
			return nil, reject(ReasonAmbiguousDevices, "the Machine holds %d devices of %s: %s; list the ones to quarantine as %s:<device>[,<device>]",
				len(candidates), strings.Join(sets.List(drivers), ", "), names(candidates), RequestQuarantine)
		}
	}

	var selected []Device
	for _, name := range requested {
		var matches []Device
		for _, d := range candidates {
			if d.Name == name {
				matches = append(matches, d)
			}
		}
		switch len(matches) {
		case 0:
			return nil, reject(ReasonUnknownDevice, "%q is not among the devices of %s the Machine holds: %s",
				name, strings.Join(sets.List(drivers), ", "), names(candidates))
		case 1:
			if !slices.Contains(selected, matches[0]) {
				selected = append(selected, matches[0])
			}
		default:
			return nil, reject(ReasonAmbiguousDevices, "%q names more than one device the Machine holds: %s",
				name, list(matches))
		}
	}
	slices.SortFunc(selected, compare)

	return selected, nil
}

// heldNote tells what the Machine does hold when none of it is of the
// configured drivers.
func heldNote(all []Device) string {
	if len(all) == 0 {
		return ""
	}

	return "; it holds " + list(all)
}

// names lists the devices by name, for an operator to pick from.
func names(ds []Device) string {
	if len(ds) == 0 {
		return "none"
	}
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Name
	}

	return strings.Join(out, ", ")
}

// list lists the devices in full.
func list(ds []Device) string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.String()
	}

	return strings.Join(out, ", ")
}

// RuleName is the name of the rule that quarantines a device. It follows
// from the device alone, so a device is never tainted by two rules of this
// operator and a request carried out twice creates nothing new. The parts
// are digested apart, since a pool name may have slashes in it.
func RuleName(d Device) string {
	return rulePrefix + digest(d.Driver+"\x00"+d.Pool+"\x00"+d.Name)
}

// digest is a short hex digest of s.
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// labelValue fits a Kubernetes object name into a label value. A name too
// long for one keeps its start and ends in a digest of the whole, which
// keeps it unique and a valid label value.
func labelValue(name string) string {
	if len(name) <= validation.LabelValueMaxLength {
		return name
	}
	d := digest(name)

	return name[:validation.LabelValueMaxLength-len(d)-1] + "-" + d
}
