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
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/signal"
)

// SourceName identifies the node condition source in logs and Events.
const SourceName = "NodeCondition"

// Source reads NVSentinel's node conditions from one workload cluster.
type Source struct {
	reader client.Reader
}

var _ signal.Source = (*Source)(nil)

// NewSource returns a Source that lists nodes through reader, which is
// expected to be a cached client for the workload cluster so that polling
// it costs nothing on the wire.
func NewSource(reader client.Reader) *Source {
	return &Source{reader: reader}
}

// Name implements signal.Source.
func (s *Source) Name() string {
	return SourceName
}

// Collect decodes every NVSentinel condition that is currently True on any
// node. Conditions reporting False are healthy and produce nothing. The
// result is sorted by node and check so that consecutive polls compare.
func (s *Source) Collect(ctx context.Context) ([]signal.Signal, error) {
	nodes := &corev1.NodeList{}
	if err := s.reader.List(ctx, nodes); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	var out []signal.Signal
	for i := range nodes.Items {
		node := &nodes.Items[i]
		for _, cond := range node.Status.Conditions {
			if cond.Status != corev1.ConditionTrue {
				continue
			}

			sig, ok := Parse(string(cond.Type), cond.Message)
			if !ok {
				continue
			}
			sig.Node = node.Name
			out = append(out, sig)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}

		return out[i].Check < out[j].Check
	})

	return out, nil
}
