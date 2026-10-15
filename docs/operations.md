# Operations

Running the operator day to day: what it records, how it answers requests,
undoing a mark, upgrading, uninstalling, and what to do when nothing
happens. To install it, see [getting-started.md](getting-started.md).

## Installing other builds

[Getting started](getting-started.md#2-install-in-dry-run-mode) installs a
release. To run the chart of a checkout with an image you built:

```sh
make docker-buildx IMG=<repository>:<tag>
helm install nvsentinel-capi-remediator charts/nvsentinel-capi-remediator \
  --namespace <namespace> --create-namespace \
  --set image.repository=<repository> --set image.tag=<tag>
```

or the kustomize manifests, which install into
`nvsentinel-capi-remediator-system`:

```sh
make deploy IMG=<repository>:<tag>
```

`make deploy` applies the checked-in manifests, which start in dry-run mode;
turn it off in a kustomize overlay of `config/default` if the change has to
survive the next deploy. Install the operator one way only: a Helm and a
kustomize install in different namespaces each hold their own
leader-election lease, so both would act.

**The dev image.** Every push to `main` that passes CI publishes its image as
`quay.io/edgestack/nvsentinel-capi-remediator:dev`, for trying a change
before it is released. The tag moves with every push, so run it with the
pull policy `Always`; its `org.opencontainers.image.revision` label names the
commit it was built from.

```sh
helm upgrade nvsentinel-capi-remediator nvsentinel-capi-remediator \
  --repo https://raw.githubusercontent.com/petasus-ai/edgestack-helm/master/ \
  --namespace nvsentinel-capi-remediator-system \
  --version <installed version> --reuse-values \
  --set image.tag=dev --set image.pullPolicy=Always
```

## What it records

Every action leaves an Event, on the Machine unless the table says
otherwise. The Machine and its annotations are gone once it is replaced; its
Events are what is left to explain it. Nothing is recorded in dry-run mode.

| Reason | Type | When |
|---|---|---|
| `MarkedForRemediation` | Warning | The Machine was marked for a replacement or a restart, or a restart mark was turned into a replacement mark. |
| `RemediationReleased` | Normal | The operator removed its restart mark: the node restarted, or the signal went away because the node condition cleared or the request was deleted. |
| `RemediationSkipped` | Warning | A signal called for remediation, but the Machine is a control plane Machine, paused, or opted out of remediation. |
| `NodeHealthReported` | Warning | A signal whose recommended action maps to a report, and every signal of a cluster in `ReportOnly` mode. |
| `RestartUnavailable` | Warning | A restart was called for, but no remediation template can carry it out (no active MachineHealthCheck selects the Machine, or one that does has no template), and the restart fallback did not turn it into a replacement. |
| `ReplaceUnavailable` | Warning | A replacement was called for, but no active MachineHealthCheck selects the Machine. |
| `ExternalRemediationRequestAnswered` | Normal | A request was answered. Recorded on the Cluster when the node has no Machine. |
| `MachineNotFound` | Warning | On the Cluster: a node with a signal has no Machine. |
| `SignalSourceSelected` | Normal | On the Cluster: the signal source was chosen, or changed. |

An Event's action field is `Remediate` or `Restart` when the Event concerns a
replacement or a restart, and otherwise `Report`, `Answer` or
`SelectSource`.

```sh
kubectl get events.events.k8s.io -A --field-selector reportingController=nvsentinel-capi-remediator
```

Two things limit what Events can tell you. Kubernetes folds Events with the
same object, reason, action and type within a few minutes into one series:
its count goes up, and the notes of the later ones are lost. And the API
server deletes Events after its `--event-ttl`, one hour by default. The
operator's log has every decision and is the record to keep.

## How requests are answered

Where a cluster serves `ExternalRemediationRequest`, NVSentinel waits for the
operator's answer in the request's `ExternalRemediationComplete` condition.

| Answer | Reason | When |
|---|---|---|
| `True` | `Restarted` | The node reports a new boot ID and is Ready again. NVSentinel takes the node back. |
| `False` | `NotRemediated` | The recommended action maps to a report. |
| `False` | `MachineNotFound` | No Machine owns the node. |
| `False` | `RemediationUnavailable` | No MachineHealthCheck can carry out the decision, even once the paused ones are unpaused. |
| `False` | `ControlPlaneMachine`, `MachineOptedOutOfRemediation` | The operator never marks such a Machine. |
| none | | A replacement: the node is deleted with its Machine, and the request with the node. |
| not yet | | The Machine is paused, being deleted or already marked; the MachineHealthChecks that could act are paused; or a restart is [held back](#a-restart-held-back). |

NVSentinel does not take the node back on `False`: the node stays cordoned
and released, on the grounds that an external system that gave up may have
left it in any state. It stays released until the request is deleted, which
NVSentinel does itself once the request's TTL has passed. To hand such a
node back sooner, once you have looked at it, delete the request in the
workload cluster:

```sh
kubectl delete externalremediationrequest <name>
```

NVSentinel's cleanup then removes the release taint, and its monitors return
to the node. That does not uncordon it: the node stays cordoned for as long
as the fault is reported, and a fault NVSentinel read from the kernel log
stays reported until the node has booted again.

Whether a new request follows depends on how the fault was detected:

- A fault read from the kernel log gets no new request: NVSentinel has
  already read that line and does not read it again.
- A fault that a returning monitor still measures, as the GPU health monitor
  does through DCGM, is reported again and gets a new request. So does any
  fault the kernel logged while the node was released.

If no new request follows, the rest is up to you: restart or replace the
node, or uncordon it if the fault turned out to be harmless, which NVSentinel
takes as the end of the quarantine. A fault that occurs again later produces
a new request, handled like any other.

## Undoing a mark

The operator removes its restart marks itself. It never removes a
replacement mark: Cluster API replaces the Machine. To stop a remediation
that has not started yet, remove the annotations from the Machine in the
management cluster:

```sh
kubectl -n <cluster-namespace> annotate machines.cluster.x-k8s.io <machine> \
  cluster.x-k8s.io/remediate-machine- \
  nvsentinel.petasus.io/remediation-reason- \
  nvsentinel.petasus.io/remediation-action- \
  nvsentinel.petasus.io/remediation-boot-id- \
  nvsentinel.petasus.io/remediation-request-
```

The operator marks the Machine again at a later poll while the signal
persists, so clear the signal first, or set
`cluster.x-k8s.io/skip-remediation` on the Machine. In
`ExternalRemediationRequest` mode, clearing the signal means deleting the
request, and opting the Machine out makes the operator answer the request
`False`, which leaves the node released.

Stopping the operator, putting it back into dry-run mode, or removing from
the Cluster the label the selector matches leaves a restart mark in place. Cluster API then keeps the
Machine unhealthy, and a provider that gives up on restarting it has it
replaced. Remove the mark as above if that is not what you want.

## Upgrading

Check what changed since your version first: the GitHub release of each
version in between, and the commits between the two tags. Then upgrade the
chart. `--reset-then-reuse-values` (Helm 3.14 or later) keeps the values you
set and takes the new chart's defaults for the rest:

```sh
helm upgrade nvsentinel-capi-remediator nvsentinel-capi-remediator \
  --repo https://raw.githubusercontent.com/petasus-ai/edgestack-helm/master/ \
  --namespace nvsentinel-capi-remediator-system \
  --version <new version> --reset-then-reuse-values
```

For the manifests install, apply the new release's `install.yaml`. It starts
in dry-run mode and without a cluster selector like every release's
manifests, so set `--dry-run=false` again afterwards, and any other argument
you changed, such as `--cluster-selector`.

Marks and answers live on the objects, so an upgrade loses none. What the
operator remembered about signals it already reported is lost with the old
pod, so each persisting signal, and each cluster's signal source, is logged
and recorded once more.

## Uninstalling

1. Where NVSentinel routes actions to requests, stop routing them first.
   Requests nobody answers keep their nodes released until their TTL.
2. Uninstall:

   ```sh
   helm uninstall nvsentinel-capi-remediator --namespace nvsentinel-capi-remediator-system
   ```

   or, for the manifests install, `kubectl delete -f` the `install.yaml` you
   applied.
3. Look for the restart marks it left:

   ```sh
   kubectl get machines.cluster.x-k8s.io -A -o custom-columns=\
   'NAMESPACE:.metadata.namespace,MACHINE:.metadata.name,'\
   'MARK:.metadata.annotations.nvsentinel\.petasus\.io/remediation-action'
   ```

   A restart mark left behind keeps its Machine unhealthy until the provider
   gives up and has it replaced. Remove the ones you do not want replaced, as
   in [Undoing a mark](#undoing-a-mark). Replacement marks can stay: Cluster
   API completes them without the operator.

## Troubleshooting

| Symptom | Likely cause | What to do |
|---|---|---|
| A cluster logs nothing at all | It does not match the cluster selector, it is paused or being deleted, or the operator cannot reach it. | Check the Cluster's labels, its `spec.paused` and its `cluster.x-k8s.io/paused` annotation. Add `--zap-log-level=debug` (chart: `extraArgs`) to see the `workload cluster is not connected, waiting` and `Cluster is paused` lines. |
| The operator does not start, or never reads a cluster | The management cluster's Cluster API does not serve `cluster.x-k8s.io/v1beta2` (it needs v1.11 or later). | Check with the command in [getting started](getting-started.md#1-check-the-requirements). |
| Signals are only reported, with `ReplaceUnavailable` or `RestartUnavailable` | No MachineHealthCheck that is not paused selects the Machine, or, for a restart, one that selects it has no template. | See [machinehealthchecks.md](machinehealthchecks.md). |
| A Machine is marked, but nothing happens | The MachineHealthCheck is over its unhealthy limit (`spec.remediation.triggerIf`), is paused, or names a template whose provider controller is not running; or the Machine is paused or opted out. | Look at the MachineHealthCheck's status (`remediationsAllowed`, conditions) and at the provider's remediation objects. |
| A cluster in `ExternalRemediationRequest` mode never reports anything | NVSentinel does not route actions to requests, or runs v1.10 to v1.12, whose janitor never releases a node to a request. | See [nvsentinel.md](nvsentinel.md#routing-actions-to-requests). |
| Nothing, or only reports, for a cluster whose NVSentinel no longer runs the janitor | The janitor's CRDs are still installed: Helm leaves them behind, and they keep the cluster in the mode they imply. | Delete them, and restart the operator: a removed resource is only noticed when the operator reconnects to the cluster. |
| A node stays cordoned after a `False` answer, or with no answer | NVSentinel keeps a node released until its request is deleted. | See [How requests are answered](#how-requests-are-answered). |
| A control plane node is never remediated | By design: the operator never marks control plane Machines. | Remediate it yourself, or through the control plane provider. |

### Log messages

Lines marked *debug* appear only with `--zap-log-level=debug`.

| The log says | Meaning |
|---|---|
| `workload cluster is not connected, waiting` (*debug*) | The Cluster's infrastructure is not provisioned yet, its kubeconfig Secret is missing, or its API server is unreachable. The cluster is read again once Cluster API's cluster cache connects. |
| `Cluster is paused, not collecting signals` (*debug*) | The Cluster is paused. |
| `selecting the signal source failed, will retry` | Discovery against the workload cluster's API server failed, as it does during an upgrade. |
| `watching the requests failed, they are read at every poll only` | The watch that has a request read at once could not be added, usually because the connection to the workload cluster was lost at that moment. Requests are still read at every poll, and the watch is tried again then. |
| `collecting signals failed, will retry`, `reading nodes failed, will retry`, `reaching the workload cluster failed, will retry`, `reading the request failed, will retry` | A read from the workload cluster failed. No mark is removed on such a poll because its signal seems gone; a request that could not be read is tried again on the next. |
| `remediation skipped` with `skip` `AlreadyMarkedForRemediation` | The Machine is already marked, and Cluster API works through the mark once nothing holds the Machine. In `ExternalRemediationRequest` mode the mark may be left from a finished restart that another request asked for. If the request now asks for a restart, that mark is removed this poll and the request is handled as for an unmarked Machine, usually with a restart of its own. Where the `replace` restart fallback applies and the Machine is neither paused nor opted out, or the request asks for a replacement, the mark becomes a replacement mark instead; see [Restart fallback](configuration.md#restart-fallback). |
| `remediation skipped` with `skip` `MachineDeleting` | The Machine is already being deleted. |
| `remediation skipped` with `skip` `EarlierRemediationNotCleared` | A restart is [held back](#a-restart-held-back). |
| `the checks still show the mark the Machine carried before, not holding the restart back` | A restart was held back for the full minute, and is asked for now. |

### A restart held back

A restart is called for, but the Machine's `HealthCheckSucceeded` condition
still names a `remediate-machine` annotation the Machine no longer carries.
That usually means a MachineHealthCheck still holds the remediation object
it created for the previous mark, and a restart asked for now would be
counted against that object. The operator holds the restart back until the
condition is rewritten.

A MachineHealthCheck with no such object to drop leaves the condition as it
is, so the wait is limited to about a minute from when the operator first
saw the condition. The minute starts again after the operator restarts, and
after the Cluster was paused or unreachable.
