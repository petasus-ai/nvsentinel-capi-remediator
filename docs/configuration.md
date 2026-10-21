# Configuration

Reference for the operator's permissions, settings, annotations, decision
table and signal sources. To install it, see
[getting-started.md](getting-started.md).

## Permissions

The operator runs in the Cluster API management cluster and needs nothing
installed in the workload clusters besides NVSentinel. It uses the `v1beta2`
version of the Cluster API resources, so the management cluster needs a
Cluster API release that serves it (v1.11 or later).

| It reads or writes | Where | Why |
|---|---|---|
| `Cluster`, `MachineHealthCheck` (get, list, watch) | management cluster | which clusters to read, and whether a Machine can be remediated |
| `Machine` (get, list, watch, patch) | management cluster | mapping nodes to Machines, and marking them |
| `Secret` (get) | management cluster | the `<cluster>-kubeconfig` Secret Cluster API keeps for each cluster. The role allows reading Secrets in every namespace, since Clusters can live in any; nothing else is read. |
| Events (`events.k8s.io`; create, patch) | management cluster | the record of what was done |
| `Lease`, core Events | its own namespace | leader election |
| `Node`, `ExternalRemediationRequest` and its status | workload clusters, through the kubeconfig above | the signals, and the answers |

The kubeconfig Cluster API generates is an administrator's, so no RBAC has to
be set up in the workload clusters.

[Device quarantine](device-quarantine.md), off by default, needs a role of its
own on top: deleting Machines, reading the KubeVirt objects, pods and
ResourceClaims behind them, and creating `DeviceTaintRule`s. The chart creates
and binds it only when the feature is on; the manifests leave it to a
kustomize component.
See [its permissions](device-quarantine.md#permissions).

## Settings

| Flag | Chart value | Default | Meaning |
|---|---|---|---|
| `--dry-run` | `dryRun` | `true` | Log every decision and write nothing: no Machine is marked, no request answered, no Event recorded. |
| `--poll-interval` | `pollInterval` | `2m` | How often each workload cluster's signals are read. Requests are the exception: the operator watches them and reads each one as soon as NVSentinel releases its node. Everything else, including the end of a restart, is noticed at the next poll. A duration such as `30s` or `1h30m`, larger units first and each at most once; the chart refuses anything else. |
| `--cluster-selector` | `clusterSelector` | empty | Label selector for the Clusters to watch, for example `environment=gpu`. Empty selects every Cluster. Clusters it does not select are never connected to. |
| `--restart-fallback` | `restartFallback` | `report` | What to do with a restart no remediation template can carry out: `report` it, or `replace` the Machine. See [Restart fallback](#restart-fallback). |
| `--decisions` | `decisions` | empty | Entries replacing those of the [decision table](#decisions): comma-separated `ACTION=decision` pairs for the flag, a map of action to decision in the chart. |
| `--device-quarantine` | `deviceQuarantine.enabled` | `false` | Carry out replace requests: quarantine the devices of the Machine's VM, then delete the Machine. See [Device quarantine](device-quarantine.md). |
| `--quarantine-drivers` | `deviceQuarantine.drivers` | empty | The DRA drivers whose devices a replace request quarantines: comma-separated for the flag, a list in the chart. Required with `--device-quarantine`. |
| `--leader-elect` | `leaderElect` | off (chart and manifests: on) | Hold a lease, so that only one replica acts. |
| `--metrics-bind-address` | `metricsBindAddress` | `0` | Address of the metrics endpoint. `0` disables it. The chart adds no port or Service for it. |
| `--health-probe-bind-address` | fixed to `:8081` | `:8081` | Address of `/healthz` and `/readyz`. |
| `--zap-*` | `extraArgs` | | Logging flags of controller-runtime's zap integration, for example `--zap-log-level=debug`. |
| `--kubeconfig` | not in the chart | `$KUBECONFIG`, then in-cluster, then `~/.kube/config` | Kubeconfig of the management cluster, for running the manager outside it, for example with `go run`. |

`extraArgs` come last, so a flag repeated there overrides the value set
above. A `clusterSelector` with several labels has to have its commas
escaped with `--set` (`--set 'clusterSelector=a=b\,c=d'`); in a values file it
needs no escaping. Every Cluster the selector matches is connected to and
keeps a node cache in the operator, so select only the clusters that run
NVSentinel.

The chart also has these values, which do not become flags:

| Chart value | Default | Meaning |
|---|---|---|
| `image.repository`, `image.tag`, `image.pullPolicy` | the release's image, `IfNotPresent` | The image. An empty tag means the chart's `appVersion`. |
| `imagePullSecrets` | empty | Pull secrets for the image. |
| `serviceAccount.create`, `.name`, `.annotations` | create, named after the release | The ServiceAccount the operator runs as. |
| `rbac.create` | `true` | Create the ClusterRole, the leader election Role and their bindings, and the device-quarantine ClusterRole when that is on. If `false`, bind the ServiceAccount yourself to roles with the rules in the chart's `files/manager-role-rules.yaml` (cluster-wide) and `files/leader-election-role-rules.yaml` (in the release namespace), and `files/device-quarantine-role-rules.yaml` (cluster-wide) for device quarantine. |
| `resources` | 10m CPU, 64Mi memory; limit 256Mi memory | Memory grows with the number of workload clusters. Set a key to `null` to drop a default. |
| `podAnnotations`, `podLabels`, `priorityClassName`, `nodeSelector`, `tolerations`, `affinity` | empty | Passed to the pod. |
| `nameOverride`, `fullnameOverride` | empty | Names of the chart's objects. |

## Restart fallback

A restart follows the restart fallback when no remediation template can
carry it out: no active MachineHealthCheck selects the Machine, or one that
selects it has no template.

- `report` (the default) reports it with a `RestartUnavailable` Event, since
  marking the Machine would replace it.
- `replace` marks the Machine for a replacement instead. That mark is never
  removed. A restart mark already on the Machine becomes a replacement mark
  too, unless the Machine is paused or opted out (see
  [Escalation](design.md#escalation)), or the restart is over and was asked
  for by the request now calling for one, which is then answered `True`
  instead. The fallback applies only while a
  MachineHealthCheck that is not paused selects the Machine; otherwise the
  restart is reported.

`--restart-fallback` sets it for all clusters. The
`nvsentinel.petasus.io/restart-fallback` annotation on a Cluster overrides it
for that cluster from the next poll on. An invalid value in the annotation is
ignored, and the Events of the signals it affects say so.

## Annotations

On a `Cluster`:

| Annotation | Set by | Meaning |
|---|---|---|
| `nvsentinel.petasus.io/restart-fallback` | you | `report` or `replace`, overriding `--restart-fallback` for this cluster. |
| `cluster.x-k8s.io/paused` (or `spec.paused`) | you or Cluster API | The cluster is not read while it is paused. |

On a `Machine`:

| Annotation | Set by | Meaning |
|---|---|---|
| `cluster.x-k8s.io/remediate-machine` | the operator | Cluster API's annotation asking for remediation. Every MachineHealthCheck that selects the Machine acts on it. |
| `nvsentinel.petasus.io/remediation-reason` | the operator | One line describing the signal behind the mark. |
| `nvsentinel.petasus.io/remediation-action` | the operator | `Restart` for a restart mark, or `Remediate` for a replacement mark. A restart mark is removed once the restart is over or its signal goes away; a replacement mark never is. |
| `nvsentinel.petasus.io/remediation-boot-id` | the operator | The node's boot ID when the restart was asked for. |
| `nvsentinel.petasus.io/remediation-request` | the operator | The `ExternalRemediationRequest` the restart was asked for. The restart answers that request and no other. |
| `cluster.x-k8s.io/paused` | you or Cluster API | The Machine is not marked while it is paused, and is looked at again on the next poll. |
| `cluster.x-k8s.io/skip-remediation` | you | The Machine is not marked while the annotation is there. |
| `nvsentinel.petasus.io/replace` | you | A request to replace the Machine once its devices are quarantined: `quarantine`, or `quarantine:<device>[,<device>]`. Read only with [device quarantine](device-quarantine.md) on. |
| `nvsentinel.petasus.io/replace-status` | the operator | How far the request got: `quarantining`, `replacing`, or `rejected: <reason>`. Remove it to try a rejected request again. |

A mark the operator set before a Machine was paused or opted out is handled
as usual: a restart mark is removed once the restart is over or its signal
goes away, and becomes a replacement mark if NVSentinel calls for a
replacement. The `replace` restart fallback is the exception: it waits until
the Machine is unpaused or opted back in (see
[Escalation](design.md#escalation)). Cluster API acts on the mark once the
Machine is unpaused or opted back in.

A Machine with the `cluster.x-k8s.io/control-plane` label is never marked.

## Decisions

NVSentinel's recommended action is mapped to a decision by a table. Its
defaults:

| Recommended action | Decision |
|---|---|
| `REPLACE_VM` | Replace |
| `RESTART_VM`, `RESTART_BM` | Restart |
| `NONE`, `COMPONENT_RESET`, `CONTACT_SUPPORT`, `RUN_FIELDDIAG`, `RUN_DCGMEUD`, `CUSTOM`, `UNKNOWN`, and any action not in the table | Report |

When a signal carries several actions, the most disruptive one wins.

`--decisions` replaces entries and adds new ones; actions it does not list
keep their default:

```text
--decisions=COMPONENT_RESET=restart,reset-fabric=restart
```

```yaml
decisions:
  COMPONENT_RESET: restart
  reset-fabric: restart
```

- **Values.** A decision is `report`, `restart` or `replace`. The flag
  accepts them in any case, the chart in lower case only. Any other value
  stops the operator at startup, and the chart from rendering.
- **Names.** Actions are matched by exact name. A name that is not one of
  NVSentinel's own actions is taken for a custom action, so a misspelled
  built-in name has no effect; the operator logs every such entry when it
  starts. Names containing a comma or `=` cannot be configured, and the chart
  also refuses names containing white space.
- **Custom actions.** Only an `ExternalRemediationRequest` carries a custom
  action's name. A node condition shows every custom action as `CUSTOM`, and
  in a request the `CUSTOM` entry applies only when the request does not name
  the custom action.
- **Which signals an entry affects.** An entry matters only for signals the
  operator acts on; see [Signal source](#signal-source). In
  `ExternalRemediationRequest` mode the operator sees only the actions
  NVSentinel routes to a request
  ([nvsentinel.md](nvsentinel.md#routing-actions-to-requests)), so an entry
  for any other action has no effect. In `ReportOnly` mode nothing is acted
  on, whatever the table says.
- **Reports decline requests.** Mapping an action that is routed to requests
  to `report` declines every such request with `False`, which leaves the node
  cordoned and released; see
  [operations.md](operations.md#how-requests-are-answered).

> [!WARNING]
> The defaults report every action that a restart or a replacement is not
> known to fix. Before you map one of them to `restart` or `replace`, check
> what raises it in your clusters: a fault that a new node inherits, or a
> false positive, then costs a node each time it is reported. A restart can
> end in a replacement too, through the `replace` fallback or when the
> provider gives up. Try a new table in dry-run mode first; removing an entry
> later does not take back a replacement already asked for.

## Signal source

The source is chosen for each cluster on every poll, from what the cluster
serves:

| The workload cluster serves | Mode | What is read |
|---|---|---|
| `ExternalRemediationRequest` (`nvsentinel.dgxc.nvidia.com/v1`) | `ExternalRemediationRequest` | Requests for the nodes NVSentinel has released. Node conditions are left to NVSentinel. |
| only the janitor's `RebootNode` (`janitor.dgxc.nvidia.com/v1alpha1`) | `ReportOnly` | Node conditions, which are reported and never acted on. |
| neither | `NodeCondition` | Node conditions, which are acted on. |

A resource installed later is noticed on the next poll. A resource that is
removed is noticed only once the operator reconnects to the cluster, and the
CRDs Helm leaves behind when NVSentinel's janitor is disabled keep the
cluster in the mode they imply. See [nvsentinel.md](nvsentinel.md) for the
NVSentinel side of each mode.
