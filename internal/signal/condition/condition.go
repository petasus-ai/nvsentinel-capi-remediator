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

// Package condition decodes the node conditions NVSentinel's Kubernetes
// platform connector publishes.
//
// The connector projects each HealthEvent onto a node condition named after
// the check: the message concatenates the events for that check, each ending
// in its recommended action, and is kept within a configurable length
// (maxNodeConditionMessageLength, 1024 by default) by first compacting the
// free-text part of each event and then, if that is not enough, cutting the
// last event and appending a marker. The recommended action survives only as
// text inside that message, so this package parses it back out. It is the
// lossier of the two sources; ExternalRemediationRequest carries the whole
// event.
package condition

import (
	"regexp"
	"strings"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal"
)

const (
	// eventSeparator joins the events of one check. The connector replaces
	// ';' inside event text with '.', so it only ever separates events, and
	// it always appends one after the last event.
	eventSeparator = ";"
	// truncationSuffix is what the connector appends after the final
	// separator when it had to drop trailing events, or cut the last one, to
	// stay within its message length. Compaction also writes "..." inside an
	// event, so only this trailing form means events were lost.
	truncationSuffix = "..."
	// actionMarker is what makes a node condition recognisably NVSentinel's.
	// Kubelet-authored conditions never carry it.
	actionMarker = "Recommended Action="
)

var (
	reAction         = regexp.MustCompile(`Recommended Action=([A-Z_]+)`)
	reErrorCode      = regexp.MustCompile(`ErrorCode:(\S+)`)
	reGpuUUID        = regexp.MustCompile(`GPU_UUID:(\S+)`)
	reTerminalAction = regexp.MustCompile(`Recommended Action=([A-Z_]+)\s*$`)
)

// Parse decodes one node condition. It reports false for conditions that
// NVSentinel did not author. The returned signal carries no node name; the
// caller knows which node the condition was read from.
func Parse(conditionType, message string) (signal.Signal, bool) {
	if !strings.Contains(message, actionMarker) {
		return signal.Signal{}, false
	}

	truncated := strings.HasSuffix(message, eventSeparator+truncationSuffix)
	evs := events(message, truncated)

	return signal.Signal{
		Origin:     signal.OriginNodeCondition,
		Check:      conditionType,
		Actions:    captures(reAction, evs),
		ErrorCodes: captures(reErrorCode, evs),
		GpuUUIDs:   captures(reGpuUUID, evs),
		Truncated:  truncated,
	}, true
}

// events splits a message into the complete events it carries. A truncated
// message may end in the prefix of an event the connector cut mid-way; its
// tokens can be anything from a partial GPU UUID to a partial action name, so
// it is dropped unless it ends in a complete, known action.
func events(message string, truncated bool) []string {
	if truncated {
		message = strings.TrimSuffix(message, truncationSuffix)
	}

	var out []string
	for _, ev := range strings.Split(message, eventSeparator) {
		if strings.TrimSpace(ev) == "" {
			continue
		}
		out = append(out, ev)
	}

	if truncated && len(out) > 0 && !isComplete(out[len(out)-1]) {
		out = out[:len(out)-1]
	}

	return out
}

// isComplete reports whether an event ends in a recommended action NVSentinel
// can emit, which every event the connector writes does. No action name is a
// prefix of another, so a cut inside the name never passes.
func isComplete(event string) bool {
	m := reTerminalAction.FindStringSubmatch(event)
	return m != nil && signal.IsKnownAction(m[1])
}

// captures returns the first submatch of every match across events, in order
// of first appearance and without duplicates. Different events of one check
// can share an action or a code, and the decision only cares which occurred.
func captures(re *regexp.Regexp, events []string) []string {
	var (
		seen = map[string]struct{}{}
		out  []string
	)
	for _, ev := range events {
		for _, m := range re.FindAllStringSubmatch(ev, -1) {
			if _, dup := seen[m[1]]; dup {
				continue
			}
			seen[m[1]] = struct{}{}
			out = append(out, m[1])
		}
	}

	return out
}
