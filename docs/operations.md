# Operations

## Installing

With Helm, a released version into the namespace of your choice:

```
helm install nvsentinel-capi-remediator nvsentinel-capi-remediator \
  --repo https://raw.githubusercontent.com/petasus-ai/edgestack-helm/master/ \
  --version <version> --namespace <namespace> --create-namespace
```

or the chart of a checkout, with an image you built:

```
helm install nvsentinel-capi-remediator charts/nvsentinel-capi-remediator \
  --namespace <namespace> --create-namespace \
  --set image.repository=<repository> --set image.tag=<tag>
```

or with the kustomize manifests, which install into
`nvsentinel-capi-remediator-system`:

```
make deploy IMG=<repository>:<tag>
```

Install it one way or the other, not both. Each release publishes its
image as `quay.io/edgestack/nvsentinel-capi-remediator:<tag>`, which the
released chart uses by default; the chart version is the tag without its
`v`. The GitHub release also carries the kustomize manifests rendered with
that image, as `install.yaml`. For an unreleased commit, build and push an
image with `make docker-buildx IMG=<repository>:<tag>`. The settings are
listed in [configuration.md](configuration.md).

## First run

The operator starts in dry-run. Nothing is written while you read its log:

```
# Helm, for a release named nvsentinel-capi-remediator
kubectl -n <namespace> logs deploy/nvsentinel-capi-remediator
# kustomize
kubectl -n nvsentinel-capi-remediator-system logs deploy/nvsentinel-capi-remediator-controller-manager
```

Writing nothing includes not answering requests. Where NVSentinel already
routes actions to requests, it has cordoned, drained and released a node by
the time the operator sees the request, and a dry run leaves it that way.
Validate before routing, or expect to delete those requests yourself.

For each watched cluster it logs the signal source it chose, and for each
signal the decision it would take: the Machine it would mark and why, the
request it would answer and how, or the reason a signal is only reported.
A signal that persists is logged once, not on every poll.

Before turning dry-run off, check for each cluster that:

- the Machines you expect to be remediated are selected by a
  MachineHealthCheck that is not paused. Without one, marking a Machine does
  nothing, so the operator reports the signal instead;
- that check has a remediation template (`spec.remediation.templateRef`) if
  you want restarts. Without a template a restart follows the restart
  fallback: reported by default, or turned into a replacement;
- signals you know to be false positives map to a report. Recommended
  actions other than a restart or a replacement always do.

Then set `dryRun=false` (chart) or `--dry-run=false`.

## What it records

Every action leaves an Event, on the Machine unless noted. The Machine and
its annotations are gone once a replacement succeeds; its Events are what is
left to explain it.

| Reason | Type | When |
|---|---|---|
| `MarkedForRemediation` | Warning | The Machine was marked for a replacement or a restart, or a restart mark was turned into a replacement mark. The action field says which. |
| `RemediationReleased` | Normal | The operator removed its restart mark: the node restarted, or the signal went away, because the node condition cleared or the request was deleted. |
| `RemediationSkipped` | Warning | A signal called for remediation, but the Machine is a control plane member, paused or opted out of remediation. |
| `NodeHealthReported` | Warning | A signal whose recommended action maps to a report, and every signal of a cluster in `ReportOnly` mode. |
| `RestartUnavailable` | Warning | A restart was called for, but no active check selects the Machine or one without a template does, and the restart fallback did not turn it into a replacement. |
| `ReplaceUnavailable` | Warning | A replacement was called for, but no active check selects the Machine. |
| `ExternalRemediationRequestAnswered` | Normal | A request was answered. Recorded on the Cluster when the node has no Machine. |
| `MachineNotFound` | Warning | On the Cluster: a node with a signal has no Machine. |
| `SignalSourceSelected` | Normal | On the Cluster: the signal source was chosen, or changed. |

```
kubectl -n <cluster-namespace> get events.events.k8s.io \
  --field-selector reportingController=nvsentinel-capi-remediator
```

Two things limit what Events can tell you. Kubernetes folds Events with the
same object, reason, action and type within a few minutes into one series:
its count goes up, and the note of the later ones is lost. And the API
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
| `False` | `RemediationUnavailable` | No MachineHealthCheck can carry out the decision, not even once paused ones are unpaused. |
| `False` | `ControlPlaneMachine`, `MachineOptedOutOfRemediation` | The operator never touches such a Machine. |
| none | | A replacement: the node is deleted with its Machine and the request with the node. Also while the Machine is paused, being deleted or already marked, while the checks that could act are paused, and while a restart is held back for the checks to drop the Machine's earlier mark. |

NVSentinel does not take the node back on `False`: it stays cordoned and
released, on the grounds that an external system which gave up may have left
it in any state. It stays released until the request is deleted, which
NVSentinel does itself once the request's TTL has passed. To hand such a
node back sooner, once you have looked at it, delete the request in the
workload cluster:

```
kubectl delete externalremediationrequest <name>
```

NVSentinel's cleanup removes the release taint, and its monitors return to
the node. That does not uncordon it. The node stays cordoned for as long as
the fault is reported, and a fault NVSentinel read from the kernel log stays
reported until the node has booted again.

Whether a new request follows depends on how the fault is found. One read
from the kernel log gets none: NVSentinel has read the line and does not
read it again. One that a returning monitor still measures, as the GPU
health monitor does through DCGM, is reported anew and gets a new request,
and so does any fault the kernel logged while the node was released. Where
none follows, what is left is yours to decide: restart or replace the node,
or uncordon it if the fault turned out to be nothing, which NVSentinel takes
as the end of the quarantine. A fault that occurs again produces a new
request, which is handled like any other.

## Undoing a mark

A restart mark is removed by the operator. A replacement mark is not: Cluster
API replaces the Machine. To stop a remediation that has not started yet,
remove the annotations from the Machine in the management cluster:

```
kubectl -n <cluster-namespace> annotate machine <machine> \
  cluster.x-k8s.io/remediate-machine- \
  nvsentinel.petasus.io/remediation-reason- \
  nvsentinel.petasus.io/remediation-action- \
  nvsentinel.petasus.io/remediation-boot-id- \
  nvsentinel.petasus.io/remediation-request-
```

The operator marks the Machine again at a later poll while the signal
persists, so clear the signal first or set
`cluster.x-k8s.io/skip-remediation` on the Machine. On a cluster read by
requests, clearing the signal means deleting the request, and opting the
Machine out makes the operator answer the request `False`, which leaves the
node released.

Stopping the operator, or putting it back into dry-run, while a Machine
carries a restart mark leaves the mark in place. Cluster API then keeps the
Machine unhealthy, and a provider that gives up on restarting it has it
replaced. Remove the mark as above if that is not what you want.

## When nothing happens

The first and the last log line below are logged at debug level; add
`--zap-log-level=debug` (chart: `extraArgs`) to see them.

| The log says | Meaning |
|---|---|
| `workload cluster is not connected, waiting` | The Cluster's infrastructure is not provisioned yet, its kubeconfig Secret is missing, or its API server is unreachable. The cluster is polled again once Cluster API's cluster cache connects. |
| `selecting the signal source failed, will retry` | Discovery against the workload API server failed, as it does during an upgrade. |
| `remediation skipped` with `skip` `AlreadyMarkedForRemediation` | The Machine is marked already and Cluster API is working through it. For a request on a cluster read by requests it can also mean that the mark is left from a restart another request asked for: that restart does not answer this one, the mark is removed in the same poll, and the request gets a restart of its own. |
| `remediation skipped` with `skip` `EarlierRemediationNotCleared` | A restart is called for, and the Machine's `HealthCheckSucceeded` condition still names a `remediate-machine` annotation it no longer carries. That usually means a MachineHealthCheck still holds the remediation request of the mark before, and a restart asked for now would be counted against that one. The restart is held back until the condition is rewritten, and for about a minute at most from when the operator first saw it, counted anew after a restart of the operator and after the Cluster was paused or out of reach: a check that has no such request to drop leaves the condition alone, so it can stay. `the checks still show the mark the Machine carried before, not holding the restart back` is logged when the minute is what ended the wait. |
| `watching the requests failed, they are read at every poll only` | The watch that has a request read at once could not be added, usually because the connection to the workload cluster was lost at that moment. Requests are still read at every poll, and the watch is tried again then. |
| `collecting signals failed, will retry`, `reading nodes failed, will retry`, `reaching the workload cluster failed, will retry`, `reading the request failed, will retry` | A read from the workload cluster failed. No mark is removed on such a poll because its signal seems gone; a request that could not be read is tried again on the next. |
| nothing at all for a cluster | It does not match `--cluster-selector`, or it is being deleted. |
| nothing, or only reports, for a cluster whose NVSentinel no longer runs the janitor | The janitor's CRDs are still installed: Helm leaves them behind, and they keep the cluster in the mode they imply. Delete them and restart the operator to have node conditions acted on again: a removed resource is only noticed when the operator reconnects to the cluster. |
| `Cluster is paused, not collecting signals` | The Cluster is paused. |

Restarting the operator loses only what it remembered about signals already
reported and the source chosen for each cluster, so each persisting signal
and each cluster's source are logged and recorded once more. Marks and
answers live on the objects and are not repeated.
