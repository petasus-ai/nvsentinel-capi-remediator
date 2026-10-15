# MachineHealthChecks

The operator never restarts or replaces a Machine itself. It marks the
Machine with Cluster API's `cluster.x-k8s.io/remediate-machine` annotation,
and every MachineHealthCheck that selects the Machine then treats it as
unhealthy and remediates it, whatever its own checks say. The
MachineHealthChecks of a cluster therefore decide what a mark does.

## What each decision needs

| Decision | Needs | Without it |
|---|---|---|
| Replace | A MachineHealthCheck that selects the Machine. | The signal is reported (`ReplaceUnavailable`). |
| Restart | A MachineHealthCheck that selects the Machine, and a remediation template (`spec.remediation.templateRef`) on every MachineHealthCheck that does. | The [restart fallback](configuration.md#restart-fallback) applies: the restart is reported (`RestartUnavailable`), or, if you choose, turned into a replacement. |

A restart needs a template on every MachineHealthCheck that selects the
Machine, because one without a template would replace the Machine at the same
time.

A paused MachineHealthCheck does not count as covering the Machine until it
is unpaused, but one without a template still rules out a restart, so the
restart fallback applies. A signal that no unpaused MachineHealthCheck can
act on is reported; an `ExternalRemediationRequest` waits if unpausing would
let one act, and is declined otherwise.

On a MachineHealthCheck with a template, a replacement also starts with the
provider's remediation. The operator never removes a replacement mark, so the
Machine stays unhealthy after that restart, and Cluster API replaces it once
the provider gives up. How long that takes depends on the template's retry
limit and timeout.

The operator logs which MachineHealthChecks cover a Machine in the `coverage`
field of its decision, for example
`MachineHealthCheck gpu-workers remediates through <kind> <name>`.

## What a remediation template has to do

The template's kind belongs to the infrastructure provider; look in its
documentation for a kind ending in `RemediationTemplate`.
[Issue #13](https://github.com/petasus-ai/nvsentinel-capi-remediator/issues/13)
tracks which providers have been tried, and reports are welcome there. The
operator relies on the provider's remediation to:

- **restart the Machine.** The operator asks for a restart only through
  MachineHealthChecks with a template, and trusts the template to restart.
- **give up in the end.** After its retries or its timeout, the provider has
  to let Cluster API replace the Machine. A restart that does not cure the
  fault, and a replacement that comes after a restart, both depend on it.

Do not let two MachineHealthChecks with templates select the same Machine.
Each creates its own remediation object, so the provider may run two
remediations at once.

## Examples

A MachineHealthCheck that replaces the Machines of one MachineDeployment:

```yaml
apiVersion: cluster.x-k8s.io/v1beta2
kind: MachineHealthCheck
metadata:
  name: <cluster>-gpu-workers
  namespace: <cluster-namespace>
spec:
  clusterName: <cluster>
  selector:
    matchLabels:
      cluster.x-k8s.io/deployment-name: <gpu-machinedeployment>
  checks:
    nodeStartupTimeoutSeconds: 600
    unhealthyNodeConditions:
      - type: Ready
        status: Unknown
        timeoutSeconds: 300
      - type: Ready
        status: "False"
        timeoutSeconds: 300
```

The same MachineHealthCheck restarts instead, given a template from the
infrastructure provider in the same namespace:

```yaml
spec:
  remediation:
    templateRef:
      apiVersion: <infrastructure-group>/<version>
      kind: <provider-remediation-template-kind>
      name: <template>
```

The `checks` are your own. The operator's mark works with any of them, and
NVSentinel's signals need no entry there.

With a ClusterClass, the topology controller creates the MachineHealthChecks
from the class: set the same `checks` and `remediation.templateRef` under
`spec.workers.machineDeployments[].healthCheck` of the ClusterClass, or for
one cluster under `spec.topology.workers.machineDeployments[].healthCheck`
of the Cluster.

## Limits that still apply

- **Unhealthy limits.** With `spec.remediation.triggerIf`
  (`unhealthyLessThanOrEqualTo` or `unhealthyInRange`), a MachineHealthCheck
  remediates nothing while more of its Machines are unhealthy than allowed.
  Marked Machines count as unhealthy, and stay marked until it remediates
  them.
- **How many at once.** A MachineDeployment's `spec.remediation.maxInFlight`
  (in a ClusterClass or topology, `healthCheck.remediation.maxInFlight`)
  limits how many of its Machines are remediated at the same time, and during
  a rollout only the newest MachineSet remediates.
- **Machine owners.** A replacement is carried out by the Machine's owner.
  The operator has been run with the Machines of MachineDeployments; other
  owners have not been tried.
- **Control plane Machines** are never marked by the operator, whatever their
  MachineHealthChecks do.

## Keeping one Machine out

Set `cluster.x-k8s.io/skip-remediation` on the Machine. MachineHealthChecks
then do not remediate it, and the operator does not mark it. A mark the
operator set before is handled as described under
[Annotations](configuration.md#annotations), and MachineHealthChecks act on
it once the opt-out is removed; to keep the Machine, remove the mark as well,
as [operations.md](operations.md#undoing-a-mark) shows.
