# Getting started

This page takes you from a management cluster with NVSentinel in some GPU
workload clusters to the operator remediating one of them, then more. Run
the `kubectl` commands against the management cluster unless a step says
otherwise.

1. [Check the requirements](#1-check-the-requirements)
2. [Install in dry-run mode](#2-install-in-dry-run-mode)
3. [Read the log](#3-read-the-log)
4. [Try a test signal](#4-try-a-test-signal) (optional)
5. [Turn remediation on](#5-turn-remediation-on)
6. [Add or remove clusters](#6-add-or-remove-clusters)

## 1. Check the requirements

**Cluster API serves `v1beta2`** (Cluster API v1.11 or later):

```sh
kubectl get crd machines.cluster.x-k8s.io \
  -o jsonpath='{.spec.versions[?(@.served==true)].name}'
```

**MachineHealthChecks select the GPU worker Machines.** The operator only
marks Machines; the MachineHealthChecks that select a Machine carry out the
remediation. List them with their remediation template, if any:

```sh
kubectl get machinehealthchecks.v1beta2.cluster.x-k8s.io -A -o custom-columns=\
'NAMESPACE:.metadata.namespace,NAME:.metadata.name,'\
'CLUSTER:.spec.clusterName,TEMPLATE:.spec.remediation.templateRef.kind'
```

A MachineHealthCheck without a template replaces the Machines it
remediates. Restarts need a template on every MachineHealthCheck that selects
a Machine. If a cluster has none yet, or you want restarts, see
[machinehealthchecks.md](machinehealthchecks.md). You can install the
operator first: in dry-run mode, the line it logs for a signal that calls
for a restart or a replacement (or for the test signal of step 4) names, in
its `coverage` field, the MachineHealthChecks that cover that Machine.

**How NVSentinel runs in each workload cluster.** The operator reads a
cluster in one of three modes, chosen from the resources the cluster serves.
Run this against each workload cluster (`clusterctl get kubeconfig <cluster>
-n <cluster-namespace>` prints its kubeconfig):

```sh
kubectl get crd externalremediationrequests.nvsentinel.dgxc.nvidia.com rebootnodes.janitor.dgxc.nvidia.com
```

| Found | Mode | What the operator reads |
|---|---|---|
| neither | `NodeCondition` | NVSentinel's node conditions. This is NVSentinel's default install, which only detects faults. |
| `externalremediationrequests…` | `ExternalRemediationRequest` | Requests for the nodes NVSentinel has handed over. Node conditions are left to NVSentinel. |
| only `rebootnodes…` | `ReportOnly` | Node conditions, which it only reports, because NVSentinel's janitor remediates by itself. |

`NodeCondition` mode is the simplest place to start.
[nvsentinel.md](nvsentinel.md) compares the modes and shows how to hand
actions over through requests. If NVSentinel in a cluster already hands
nodes over through `ExternalRemediationRequest`, read
[Before you route actions to requests](#before-you-route-actions-to-requests)
before you install.

## 2. Install in dry-run mode

In dry-run mode, the default, the operator logs the decision it would take
for every signal and writes nothing. Limit it to the Clusters you label, and
label one:

```sh
helm install nvsentinel-capi-remediator nvsentinel-capi-remediator \
  --repo https://raw.githubusercontent.com/petasus-ai/edgestack-helm/master/ \
  --namespace nvsentinel-capi-remediator-system --create-namespace \
  --set clusterSelector=nvsentinel-remediation=enabled

kubectl -n <cluster-namespace> label clusters.cluster.x-k8s.io <cluster> nvsentinel-remediation=enabled
```

If a cluster template or a GitOps tool manages the Cluster, add the label
there, so that it survives the next sync.

Add `--version <version>` to pin a release; without it Helm installs the
latest. A selector with several labels needs its commas escaped on the
command line (`--set 'clusterSelector=environment=gpu\,team=ml'`), or Helm
silently keeps only the first label; in a values file no escaping is needed.
Without a selector the operator connects to every Cluster, including those
without GPUs, and keeps a node cache for each.

Without Helm, apply the manifests of the latest release. They install the
operator into `nvsentinel-capi-remediator-system`, in dry-run mode and without
a cluster selector:

```sh
kubectl apply -f https://github.com/petasus-ai/nvsentinel-capi-remediator/releases/latest/download/install.yaml
```

To limit it, add `--cluster-selector=nvsentinel-remediation=enabled` to the
container's arguments, the way step 5 changes `--dry-run`.
[configuration.md](configuration.md) lists every setting.

## 3. Read the log

```sh
kubectl -n nvsentinel-capi-remediator-system logs deploy/nvsentinel-capi-remediator -f
```

For the manifests install, the Deployment is
`nvsentinel-capi-remediator-controller-manager`. The log is JSON, one object
per line; the `msg` field tells the lines apart.

| `msg` | What it means |
|---|---|
| `starting manager` | The operator started. The line lists its settings, `dryRun` among them. |
| `signal source selected` | A cluster was connected and read, in the `mode` given. Logged once per cluster after a start, and again when the mode changes. |
| `would mark Machine for remediation, asking for a restart` (or `a replacement`) | A signal would be acted on. `node`, `machine`, `decision` and `coverage` say what, and through which MachineHealthChecks. |
| `would answer ExternalRemediationRequest` | A request would be answered, with the `answerMessage` given. |
| `would release Machine from remediation, …` | A restart mark would be removed. |
| `remediation skipped` | The Machine is left alone; `skip` says why. See [Log messages](operations.md#log-messages). |
| any other line with a `decision` | A signal that is only reported. The message says why, for example that no MachineHealthCheck selects the Machine. |
| `health signal cleared` | A signal went away. |

A signal that persists is logged once, not on every poll. Other messages,
and what to do about them, are listed under
[Troubleshooting](operations.md#troubleshooting).

**A labeled cluster that logs nothing** is not connected, or paused. Those
lines are at debug level. To see them, turn on debug logging:

```sh
helm upgrade nvsentinel-capi-remediator nvsentinel-capi-remediator \
  --repo https://raw.githubusercontent.com/petasus-ai/edgestack-helm/master/ \
  --namespace nvsentinel-capi-remediator-system \
  --version <installed version> --reuse-values \
  --set 'extraArgs={--zap-log-level=debug}'
```

`helm get metadata nvsentinel-capi-remediator -n nvsentinel-capi-remediator-system`
shows the installed version. For the manifests install, add
`--zap-log-level=debug` to the container's arguments instead.

**No Events are recorded in dry-run mode.** The log is the only record.

## 4. Try a test signal

Without a real fault there is nothing to decide. On a cluster in
`NodeCondition` mode you can raise a signal by hand: the operator reads any
node condition that is `True` and whose message carries
`Recommended Action=`.

> [!CAUTION]
> Do this only in dry-run mode. With remediation on, the operator would mark
> the Machine, and since nothing clears a hand-made condition, the restart
> mark would stay after the restart until the provider gives up and Cluster
> API replaces the Machine.

Against the workload cluster, on a GPU worker node:

```sh
kubectl patch node <node> --subresource=status --type=strategic -p '
  {"status": {"conditions": [{"type": "RemediatorTest", "status": "True", "reason": "Test",
    "message": "ErrorCode:TEST GPU_UUID:GPU-test Recommended Action=RESTART_VM;"}]}}'
```

Within a poll (two minutes by default) the log shows the decision for node
`<node>`: a `would mark Machine for remediation, asking for a restart` line
when its MachineHealthChecks can restart it, or a line saying why the restart
is only reported. `REPLACE_VM` in place of `RESTART_VM` tries the
replacement path. Remove the condition afterwards:

```sh
kubectl patch node <node> --subresource=status --type=strategic -p '
  {"status": {"conditions": [{"type": "RemediatorTest", "$patch": "delete"}]}}'
```

The log then shows `health signal cleared`. On a cluster in
`ExternalRemediationRequest` mode the signals come from NVSentinel's own
pipeline, so a test there needs a fault NVSentinel reports.

## 5. Turn remediation on

Turning dry-run mode off applies to every cluster the selector matches;
there is no dry run per cluster. Before you do, check for each of them that:

- the GPU worker Machines are selected by a MachineHealthCheck that is not
  paused; without one the operator only reports;
- for restarts, every MachineHealthCheck that selects them has a remediation
  template; without one a restart follows the
  [restart fallback](configuration.md#restart-fallback), which reports it by
  default;
- the decisions in the log are the ones you want. Faults you know to be false
  positives should map to a report; see [Decisions](configuration.md#decisions).

Then turn dry-run mode off. With Helm:

```sh
helm upgrade nvsentinel-capi-remediator nvsentinel-capi-remediator \
  --repo https://raw.githubusercontent.com/petasus-ai/edgestack-helm/master/ \
  --namespace nvsentinel-capi-remediator-system \
  --version <installed version> --reuse-values --set dryRun=false
```

For the manifests install, change `--dry-run=true` to `--dry-run=false` in
the container's arguments:

```sh
kubectl -n nvsentinel-capi-remediator-system edit deployment nvsentinel-capi-remediator-controller-manager
```

From then on the log says `Machine marked for remediation`,
`ExternalRemediationRequest answered` and `Machine released from remediation`
where it said `would …`, and every action is recorded as an Event:

```sh
kubectl get events.events.k8s.io -A \
  --field-selector reportingController=nvsentinel-capi-remediator
```

The Machines the operator has marked, and for what:

```sh
kubectl get machines.cluster.x-k8s.io -A -o custom-columns=\
'NAMESPACE:.metadata.namespace,MACHINE:.metadata.name,'\
'MARK:.metadata.annotations.nvsentinel\.petasus\.io/remediation-action'
```

`Restart` marks are removed once the restart is over; `Remediate` marks stay
until Cluster API has replaced the Machine. [operations.md](operations.md)
explains the Events, the answers to requests, and how to undo a mark.

### Before you route actions to requests

Where NVSentinel hands nodes over through `ExternalRemediationRequest`, it
cordons and drains a node, creates the request, and then releases the node
to it. In dry-run mode the operator does not answer, so the node stays
released until the request is deleted or its TTL passes, 14 days in the
example configuration. Hand actions over through requests only on a cluster
where the operator already remediates:

1. Run the dry run while NVSentinel only detects faults (`NodeCondition` mode).
2. Prepare the MachineHealthChecks.
3. Turn remediation on.
4. Route NVSentinel's actions to requests, as
   [nvsentinel.md](nvsentinel.md#routing-actions-to-requests) shows.

If NVSentinel already routes actions to requests on a cluster, keep the dry
run short there, and delete the requests it leaves unanswered once you have
looked at their nodes (`kubectl delete externalremediationrequest <name>`
against the workload cluster).

## 6. Add or remove clusters

With dry-run mode off, a cluster you label is remediated from its first
poll, so check its MachineHealthChecks as in step 1 before you label it:

```sh
kubectl -n <cluster-namespace> label clusters.cluster.x-k8s.io <cluster> nvsentinel-remediation=enabled
```

To take a cluster out, first stop NVSentinel there from routing actions to
requests, if it does. Then remove the cluster's label: the operator forgets
the cluster and no longer touches it. Last, remove the restart marks left on
its Machines (see [Undoing a mark](operations.md#undoing-a-mark)), since a
restart mark left there ends in a replacement. To keep the operator off one
Machine, set
`cluster.x-k8s.io/skip-remediation` on the Machine. A cluster can have its
own [restart fallback](configuration.md#restart-fallback); every other
setting applies to all clusters.
