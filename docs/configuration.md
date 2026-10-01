# Configuration

## What the operator needs

It runs in the Cluster API management cluster and needs nothing installed in
the workload clusters besides NVSentinel. It uses the `v1beta2` version of
the Cluster API resources, so the management cluster needs a Cluster API
release that serves it (v1.11 or later).

| It reads or writes | Where | Why |
|---|---|---|
| `Cluster`, `MachineHealthCheck` (get, list, watch) | management cluster | which clusters to poll, and whether a Machine can be remediated |
| `Machine` (get, list, watch, patch) | management cluster | mapping nodes to Machines, and marking them |
| `Secret` (get) | management cluster | the `<cluster>-kubeconfig` Secret Cluster API keeps for each cluster. The role allows reading Secrets in every namespace, since Clusters can live in any; nothing else is read. |
| Events (`events.k8s.io`, create, patch) | management cluster | the record of what was done |
| `Lease`, core Events | its own namespace | leader election |
| `Node`, `ExternalRemediationRequest` and its status | workload clusters, through the kubeconfig above | the signals, and the answer |

The kubeconfig Cluster API generates is an administrator's, so no RBAC has to
be set up in the workload clusters.

## Settings

| Flag | Chart value | Default | Meaning |
|---|---|---|---|
| `--dry-run` | `dryRun` | `true` | Log every decision and write nothing: no Machine is marked, no request answered, no Event recorded. |
| `--poll-interval` | `pollInterval` | `2m` | How often each workload cluster's signals are read. |
| `--cluster-selector` | `clusterSelector` | empty | Label selector for the Clusters to watch, e.g. `environment=gpu`. Empty selects every Cluster. Clusters it does not select are never connected to. |
| `--restart-fallback` | `restartFallback` | `report` | What to do with a restart no remediation template can carry out: `report` it, or `replace` the Machine. |
| `--leader-elect` | `leaderElect` | off (chart and kustomize: on) | Hold a lease so that only one replica acts. |
| `--metrics-bind-address` | `metricsBindAddress` | `0` | Address of the metrics endpoint. `0` disables it. |
| `--health-probe-bind-address` | fixed to `:8081` | `:8081` | Address of `/healthz` and `/readyz`. |
| `--zap-*` | `extraArgs` | | Logging flags of controller-runtime's zap integration. |
| `--kubeconfig` | not for the chart | in-cluster | Kubeconfig of the management cluster, for running the manager outside it, e.g. with `go run`. `$KUBECONFIG` is honoured too. |

## Annotations

On a `Cluster`:

| Annotation | Set by | Meaning |
|---|---|---|
| `nvsentinel.petasus.io/restart-fallback` | you | `report` or `replace`, overriding `--restart-fallback` for this cluster from the next poll on. An invalid value is ignored, and the Events of the signals it affects say so. |
| `cluster.x-k8s.io/paused` (or `spec.paused`) | you / Cluster API | The cluster is not polled while paused. |

On a `Machine`:

| Annotation | Set by | Meaning |
|---|---|---|
| `cluster.x-k8s.io/remediate-machine` | the operator | Cluster API's request to remediate. Every MachineHealthCheck selecting the Machine acts on it. |
| `nvsentinel.petasus.io/remediation-reason` | the operator | A one-line description of the signal behind the mark. |
| `nvsentinel.petasus.io/remediation-action` | the operator | `Remediate` or `Restart`. A `Restart` mark is removed again once the restart is over; a `Remediate` mark never is. |
| `nvsentinel.petasus.io/remediation-boot-id` | the operator | The node's boot ID when a restart was asked for. |
| `cluster.x-k8s.io/paused` | you / Cluster API | The Machine is left alone and looked at again on the next poll. |
| `cluster.x-k8s.io/skip-remediation` | you | The Machine is never marked. |

A Machine carrying the `cluster.x-k8s.io/control-plane` label is never marked
either.

## Decisions

The mapping from NVSentinel's recommended action to a decision is built in:

| Recommended action | Decision |
|---|---|
| `REPLACE_VM` | Replace |
| `RESTART_VM`, `RESTART_BM` | Restart |
| `NONE`, `COMPONENT_RESET`, `CONTACT_SUPPORT`, `RUN_FIELDDIAG`, `RUN_DCGMEUD`, `CUSTOM`, anything unknown | Report |

When a signal carries several actions, the most disruptive one wins.

## Signal source

The source is chosen per cluster on every poll, from what the cluster serves:

| The workload cluster serves | Mode | What is read |
|---|---|---|
| `externalremediationrequests.nvsentinel.dgxc.nvidia.com/v1` | `ExternalRemediationRequest` | requests whose node NVSentinel has released; node conditions are left to NVSentinel |
| only the janitor's `rebootnodes.janitor.dgxc.nvidia.com/v1alpha1` | `ReportOnly` | node conditions, reported and never acted on |
| neither | `NodeCondition` | node conditions, acted on |

A resource installed later is noticed on the next poll. One that is removed
is only noticed once the operator reconnects to the cluster, and the CRDs
Helm leaves behind when NVSentinel's janitor is disabled keep the cluster in
the mode they imply.

See [nvsentinel.md](nvsentinel.md) for the NVSentinel side of each mode.
