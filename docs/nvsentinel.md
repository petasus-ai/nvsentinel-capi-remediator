# The NVSentinel side

The operator adds nothing to a workload cluster. What it does there depends
on how NVSentinel is deployed.

| NVSentinel deployment | What the operator does |
|---|---|
| Detection only: health monitors and the platform connector, no janitor. Faults show as node conditions. | Reads the node conditions and acts on them. |
| Remediation pipeline with the janitor, actions routed to `ExternalRemediationRequest` (NVSentinel v1.13 or later). | Reads the requests and answers them. Node conditions are left to NVSentinel. |
| Remediation pipeline with the janitor acting itself, on a version that serves the request resource (v1.10 or later). | Nothing: there are no requests to read, and node conditions are left to NVSentinel. On v1.10 to v1.12 that holds even when actions are routed to requests, since the janitor never releases a node to one. |
| Remediation pipeline with the janitor acting itself, on a version without the request resource (before v1.10). | Reads the node conditions and only reports them, so that two systems never remediate one node. |

Which of these a cluster is in is detected from the resources it serves; see
[configuration.md](configuration.md#signal-source).

## Routing actions to requests

With the remediation pipeline, NVSentinel cordons and drains a faulty node
and then creates the maintenance resource configured for the recommended
action. Pointing the actions this operator handles at
`ExternalRemediationRequest` is what hands them over. The part of the values
of NVSentinel's Helm chart (v1.23.0) that does so follows; NVSentinel's own
prerequisites, such as cert-manager, apply as they do without it.

```yaml
global:
  faultQuarantine:
    enabled: true
  nodeDrainer:
    enabled: true
  faultRemediation:
    enabled: true
  janitor:
    enabled: true
  mongodbStore:
    enabled: true

fault-remediation:
  maintenance:
    actions:
      RESTART_VM: &request
        apiGroup: nvsentinel.dgxc.nvidia.com
        version: v1
        kind: ExternalRemediationRequest
        scope: Cluster
        completeConditionType: ExternalRemediationComplete
        templateFileName: extrr.yaml
        equivalenceGroup: external-remediation
      RESTART_BM: *request
      REPLACE_VM: *request
      # Nothing is done about a GPU reset: see the notes below.
      COMPONENT_RESET: null
    templates:
      extrr.yaml: |
        apiVersion: nvsentinel.dgxc.nvidia.com/v1
        kind: ExternalRemediationRequest
        metadata:
          name: extrr-{{ .HealthEventID }}
          labels:
            app.kubernetes.io/managed-by: nvsentinel
          annotations:
            nvsentinel.nvidia.com/trace-id: {{ printf "%q" .TraceID }}
            nvsentinel.nvidia.com/span-id: {{ printf "%q" .SpanID }}
            nvsentinel.nvidia.com/ttl: "336h"
        spec:
          healthEvent:
            nodeName: {{ printf "%q" .HealthEvent.NodeName }}
            agent: {{ printf "%q" .HealthEvent.Agent }}
            componentClass: {{ printf "%q" .HealthEvent.ComponentClass }}
            checkName: {{ printf "%q" .HealthEvent.CheckName }}
            isFatal: {{ .HealthEvent.IsFatal }}
            recommendedAction: {{ printf "%q" .HealthEvent.RecommendedAction.String }}
            customRecommendedAction: {{ printf "%q" .HealthEvent.CustomRecommendedAction }}
            errorCode: [{{ range $i, $c := .HealthEvent.ErrorCode }}{{ if $i }}, {{ end }}{{ printf "%q" $c }}{{ end }}]
            entitiesImpacted: [{{ range $i, $e := .HealthEvent.EntitiesImpacted }}{{ if $i }}, {{ end }}{entityType: {{ printf "%q" $e.EntityType }}, entityValue: {{ printf "%q" $e.EntityValue }}}{{ end }}]
            id: {{ if .HealthEvent.Id }}{{ printf "%q" .HealthEvent.Id }}{{ else }}{{ printf "%q" .HealthEventID }}{{ end }}
            message: {{ printf "%q" .HealthEvent.Message }}
```

Notes on that configuration:

- **One equivalence group for all three actions.** NVSentinel's janitor marks
  a released node with a taint that is unique per node, so a node can have
  one open request at a time. A shared group keeps fault-remediation from
  creating a second one while the first is open.
- **The template is a Go template that fault-remediation renders.** If the
  values pass through another templating step first, as with Cluster API's
  Helm add-on provider, its `{{ }}` have to be escaped for that step.
- **Actions left at the chart defaults are remediated by the janitor.** The
  defaults map `COMPONENT_RESET`, `RESTART_VM` and `RESTART_BM` to the
  janitor's own `RebootNode` and `REPLACE_VM` to `TerminateNode`, which need
  the janitor's cloud provider. Set an action to `null`, as the example does
  for `COMPONENT_RESET`, to have nothing done about it: the node then stays
  cordoned until its fault clears.
- **The janitor expects its cloud provider.** Where the provider is not
  deployed, turn off the janitor's `rebootNode`, `terminateNode` and
  `gpuReset` controllers (`janitor.config.controllers.<name>.enabled: false`)
  and the provider's TLS and authentication
  (`janitor.config.cspProvider.tls.enabled` and `.auth.enabled`), or the
  janitor pod waits for a certificate nobody issues. The controller that
  handles requests needs none of them.
- **Requests are deleted after the TTL** in the annotation, 14 days here,
  whether or not they were answered. Deleting a request hands its node back
  to NVSentinel, so that is also how long a node nobody remediates stays
  released.

## What happens to a released node

1. fault-remediation creates the request. The janitor taints the node with
   `nvsentinel.dgxc.nvidia.com/external-remediation=<request>:NoSchedule`,
   labels it `nvsentinel.dgxc.nvidia.com/managed=false`, which takes
   NVSentinel's monitors off it, and sets the request's
   `NVSentinelOwnershipReleased` condition to `True`.
2. The operator watches the requests, so it reads this one as soon as the
   node is released, decides, and marks the Machine in the management
   cluster.
3. For a restart, once the node is Ready with a new boot ID the operator
   sets `ExternalRemediationComplete` to `True`, at its next poll. The
   janitor removes the taint and the label, the monitors return, and
   NVSentinel uncordons the node once its checks pass.
4. For a replacement the node is deleted with its Machine, and the request
   goes with the node.

An answer of `False`, and a request nobody answers, leave the node released
until the request is deleted, by you or by its TTL. See
[operations.md](operations.md#how-requests-are-answered) for what the
operator answers and how to take such a node back.
