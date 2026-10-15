# The NVSentinel side

The operator adds nothing to a workload cluster. What it does there depends
on how NVSentinel is deployed, which this page covers.

NVSentinel components named on this page:

- **health monitors**, such as the GPU health monitor (through DCGM) and the
  syslog health monitor, which detect faults;
- the **platform connector**, which publishes faults as node conditions;
- **fault-quarantine** and **node-drainer**, which cordon and drain a faulty
  node;
- **fault-remediation**, which creates the maintenance object configured for
  a fault's recommended action;
- the **janitor**, which carries out NVSentinel's own maintenance objects
  (`RebootNode`, `TerminateNode`, `GPUReset`) and releases nodes to
  `ExternalRemediationRequest`;
- the **metadata collector**, which collects the GPU topology and records
  which pods use which GPU.

## Choosing a mode

| | Detection only | Actions routed to requests |
|---|---|---|
| NVSentinel deploys | its detection components only (its default install) | the remediation pipeline as well: quarantine, drain, fault-remediation, janitor, and its datastore |
| Operator mode | `NodeCondition` | `ExternalRemediationRequest` |
| Before the operator acts | nothing: the node keeps running its pods | NVSentinel cordons and drains the node, then releases it |
| What the operator reads | node conditions | requests, as soon as a node is released |
| What NVSentinel gets back | nothing; it clears the condition once the fault is gone | an answer on the request; on `True` it takes the node back |

Detection only is the simplest start, and the operator acts on it fully. Routing
actions to requests adds NVSentinel's quarantine and drain before every
remediation, and an answer for each one. Route actions to requests only on a
cluster where the operator already remediates; see
[getting-started.md](getting-started.md#before-you-route-actions-to-requests).

How each NVSentinel deployment is read, by version:

| NVSentinel deployment | What the operator does |
|---|---|
| Detection only: health monitors and the platform connector, no janitor. Faults show as node conditions. | Reads the node conditions and acts on them (`NodeCondition` mode). |
| Remediation pipeline with the janitor, actions routed to `ExternalRemediationRequest` (NVSentinel v1.13 or later). | Reads the requests and answers them (`ExternalRemediationRequest` mode). Node conditions are left to NVSentinel. |
| Remediation pipeline with the janitor acting itself, on a version that serves the request resource (v1.10 or later). | Nothing: there are no requests to read, and node conditions are left to NVSentinel. On v1.10 to v1.12 that holds even when actions are routed to requests, since the janitor never releases a node to one. |
| Remediation pipeline with the janitor acting itself, on a version without the request resource (before v1.10). | Reads the node conditions and only reports them, so that two systems never remediate one node (`ReportOnly` mode). |

The operator tells these apart by the resources a cluster serves; see
[configuration.md](configuration.md#signal-source). The values below were
written for NVSentinel's chart v1.26.0; check the keys against the chart of
your version.

## Routing actions to requests

> [!IMPORTANT]
> Once actions are routed to requests, NVSentinel cordons, drains and
> releases a node before the operator sees it. An operator in dry-run mode
> does not answer, so the node stays released until the request is deleted
> or its TTL passes. Route actions to requests only where the operator
> already remediates.

With the remediation pipeline, NVSentinel cordons and drains a faulty node
and then creates the maintenance object configured for the recommended
action. To hand an action to this operator, route it to
`ExternalRemediationRequest`. The excerpt below of the values for
NVSentinel's Helm chart (v1.26.0) does that. NVSentinel's own prerequisites,
such as cert-manager, still apply.

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

- **One equivalence group for all three actions.** NVSentinel's janitor
  taints a released node with a taint that is unique per node, so a node can
  have one open request at a time. A shared group keeps fault-remediation from
  creating a second one while the first is open.
- **The template is a Go template that fault-remediation renders.** If the
  values pass through another templating step first, as with Cluster API's
  Helm add-on provider, its `{{ }}` have to be escaped for that step.
- **Actions left at the chart defaults are remediated by the janitor.** The
  defaults map `COMPONENT_RESET`, `RESTART_VM` and `RESTART_BM` to the
  janitor's own `RebootNode` and `REPLACE_VM` to `TerminateNode`, which need
  the janitor's CSP provider (its cloud provider plugin, not a Cluster API
  infrastructure provider). Set an action to `null`, as the example does
  for `COMPONENT_RESET`, to have nothing done about it: the node then stays
  cordoned until its fault clears. [A GPU reset](#a-gpu-reset) has the
  alternatives for that action.
- **The janitor expects its CSP provider.** Where that provider is not
  deployed, turn off:
  - the janitor's `rebootNode` and `terminateNode` controllers, and
    `gpuReset` unless you use it (it needs no provider), with
    `janitor.config.controllers.<name>.enabled: false`;
  - the provider's TLS and authentication, with
    `janitor.config.cspProvider.tls.enabled` and
    `janitor.config.cspProvider.auth.enabled`.

  Otherwise the janitor pod waits for a certificate that nothing issues. The
  controller that handles requests needs none of these.
- **Requests are deleted after the TTL** in the annotation, 14 days here,
  whether or not they were answered. Deleting a request hands its node back
  to NVSentinel, so that is also how long a node nobody remediates stays
  released.
- **A fault that outlasts its remediation gets a new request.** Once a
  request is answered `True` the monitors return, report what they still
  see, and fault-remediation opens another request for an event newer than
  the answered one, without limit by default. Set
  `fault-remediation.maxRemediationAttempts` to end that: after that many
  attempts within one quarantine NVSentinel labels the node
  `remediation-failed` and leaves it cordoned, not released. A request the
  operator declined counts as an attempt, and so does one that could not be
  created. The actions that share an equivalence group share the count, so
  with `maxRemediationAttempts: 1` a replacement asked for after a restart
  would also be refused.

## A GPU reset

`COMPONENT_RESET` is NVSentinel's recommendation to reset one GPU. This
operator does not reset GPUs, which leaves three things to do with it.

### Option 1: have NVSentinel's janitor reset the GPU

The janitor's `GPUReset` takes
the GPU operator's services off the node, runs `nvidia-smi --gpu-reset` for
that GPU in a Job on the node, and puts the services back. It needs no CSP
provider, and it has been seen to work on a GPU passed through to a KubeVirt
virtual machine. The operator has no part in it, except when a reset fails:
NVSentinel then reports a fault that asks for a restart, which goes to a
request like any other. For chart v1.26.0, merge the following into the
values above; its `COMPONENT_RESET` entry replaces the `null` under the same
`actions` key:

```yaml
fault-remediation:
  maintenance:
    actions:
      COMPONENT_RESET:
        apiGroup: janitor.dgxc.nvidia.com
        version: v1alpha1
        kind: GPUReset
        scope: Cluster
        completeConditionType: Complete
        templateFileName: gpureset.yaml
        equivalenceGroup: gpu-reset
        impactedEntityScope: GPU_UUID
        # Held back while a request for the node is open. The group has to
        # be one another action defines.
        supersedingEquivalenceGroups: [external-remediation]
    templates:
      gpureset.yaml: |
        apiVersion: janitor.dgxc.nvidia.com/v1alpha1
        kind: GPUReset
        metadata:
          name: gpureset-{{ .HealthEventID }}
        spec:
          nodeName: {{ printf "%q" .HealthEvent.NodeName }}
          selector:
            uuids:
              - {{ printf "%q" .ImpactedEntityScopeValue }}

node-drainer:
  # Drain only the pods using the GPU.
  partialDrainEnabled: true

gpu-health-monitor:
  dcgmConnectivity:
    runtimeDebounce:
      failureThreshold: 4

janitor:
  config:
    controllers:
      gpuReset:
        enabled: true
```

#### What to check before relying on it

- **Where the GPU operator runs.** The janitor's built-in description of it
  looks in the `gpu-operator` namespace. Anywhere else, give
  `janitor.config.controllers.gpuReset.serviceManager.spec` in full:
  `namespace`, `managerSelector`, `teardownTimeout`, `restoreTimeout`, and
  for each app `appSelector`, `nodeLabel`, `enabledValue` and
  `disabledValue`. Nothing in a description given this way has a default:
  without `enabledValue` and `disabledValue` the node's
  `nvidia.com/gpu.deploy.*` labels are emptied for the reset and never set
  back, and the services stay off the node.
- **Which services run.** The janitor considers a reset done only once every
  service in the description has a ready pod on the node again. The built-in
  description lists the device plugin, so on a cluster whose GPUs a DRA
  (Dynamic Resource Allocation) driver hands out, where the GPU operator runs
  no device plugin, a reset that worked ends with `RestoreTimeoutExceeded`
  ten minutes later. List the services that do run: DCGM, the DCGM exporter
  and GPU feature discovery.
- **DCGM is away during a reset.** The GPU health monitor reports the first
  failed poll of DCGM as a fault of its own, which can quarantine the node
  again after a reset. The debounce above makes that four polls in a row.
- **What the reset Job needs.** It runs on the workload node, privileged and
  with host paths, in the janitor's namespace, so that namespace's pod
  security has to admit it. It asks for the runtime class `nvidia`, runs
  `nvidia-smi` from the GPU operator's driver container at
  `/run/nvidia/driver` (for a driver installed on the host see
  `janitor.config.controllers.gpuReset.resetJob.hostDriverRootPath`), and pulls
  `ghcr.io/nvidia/nvsentinel/gpu-reset` on every reset. Where one of these
  does not hold, the Job ends before it reports anything, fault-remediation
  reads the `GPUReset` as done either way, and the node stays cordoned with
  nothing acting on it.
- **Which pods are drained.** With `partialDrainEnabled`, only the pods that
  use the GPU are drained. NVSentinel identifies them from an annotation its
  metadata collector keeps on each pod, based on the kubelet's pod
  resources; claims of the `gpu.nvidia.com` DRA driver are understood. A pod
  with a claim but without that annotation is left running, and the reset
  then fails on a busy GPU. A device-plugin pod without it holds up the drain
  until it gets one.

### Option 2: have nothing done

Set the action to `null`, as the first example does. The node stays cordoned
and drained until something clears the fault.

### Option 3: have the node restarted

Route `COMPONENT_RESET` to a request like the other three, in the same
equivalence group, leave `partialDrainEnabled` off, and tell the operator to
treat the action as a restart. By default it only reports it, which declines
the request:

```text
--decisions=COMPONENT_RESET=restart
```

or, with the chart:

```yaml
decisions:
  COMPONENT_RESET: restart
```

## What happens to a released node

1. NVSentinel's fault-remediation creates the request. The janitor taints the node with
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
   NVSentinel uncordons the node once its checks pass. A restart answers
   the request it was asked for and no other: a request raised for the node
   after it is back gets a restart of its own.
4. For a replacement the node is deleted with its Machine, and the request
   goes with the node.

An answer of `False`, and a request nobody answers, leave the node released
until the request is deleted, by you or by its TTL. See
[operations.md](operations.md#how-requests-are-answered) for what the
operator answers and how to take such a node back.
