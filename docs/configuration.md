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
| `--decisions` | `decisions` | empty | Entries replacing those of the [decision table](#decisions), as comma-separated `ACTION=decision` pairs; in the chart, a map of action to decision. |
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

NVSentinel's recommended action is mapped to a decision by a table. Its
defaults:

| Recommended action | Decision |
|---|---|
| `REPLACE_VM` | Replace |
| `RESTART_VM`, `RESTART_BM` | Restart |
| `NONE`, `COMPONENT_RESET`, `CONTACT_SUPPORT`, `RUN_FIELDDIAG`, `RUN_DCGMEUD`, `CUSTOM`, anything unknown | Report |

When a signal carries several actions, the most disruptive one wins.

`--decisions` replaces entries and adds new ones; actions it does not list
keep their default:

```
--decisions=COMPONENT_RESET=restart,reset-fabric=restart
```

```yaml
decisions:
  COMPONENT_RESET: restart
  reset-fabric: restart
```

- A decision is `report`, `restart` or `replace`. The flag takes them in any
  case, the chart in lower case only. Anything else stops the manager at
  startup, and the chart from rendering.
- An action is matched by its exact name. One that NVSentinel does not
  recommend itself is taken for the name of a custom action, so a misspelt
  built-in name changes nothing; the manager lists such entries in its log
  when it starts. A name with a comma or `=` in it cannot be listed, and the
  chart refuses white space in one as well.
- A custom action is known by its name only in an
  `ExternalRemediationRequest`, and there the `CUSTOM` entry only applies to
  a request that leaves the name out. A node condition carries `CUSTOM` for
  every custom action.
- An entry applies to the signals the operator reads; see
  [Signal source](#signal-source). Where it reads requests, those are the
  actions NVSentinel routes to a request ([nvsentinel.md](nvsentinel.md)),
  and an entry for any other action has no effect. Where NVSentinel's
  janitor remediates, nothing is acted on whatever the table says.
- Mapping an action that is routed to requests to `report` declines every
  such request: the answer is `False`, and the node stays cordoned and
  released until the request is deleted; see
  [operations.md](operations.md#how-requests-are-answered).
- The defaults report everything a restart or a replacement is not known to
  fix. Before mapping such an action to one, check what raises it in your
  clusters: a fault that a new node inherits, or a false positive, then costs
  a node each time it is reported. A restart can end in a replacement too:
  when no remediation template can carry it out and the restart fallback is
  `replace`, or when the provider gives up on restarting. Try a new table in
  dry run first: taking an entry out again does not take back a replacement
  that was already asked for.

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
