# Design

How the operator works, step by step, and why it works that way. To install
and run it, start with [getting-started.md](getting-started.md).

Terms used here:

- A **request** is NVSentinel's `ExternalRemediationRequest`, the object
  through which NVSentinel hands a node to an external system.
- A **mark** is the `cluster.x-k8s.io/remediate-machine` annotation the
  operator sets on a Machine, together with its own
  `nvsentinel.petasus.io/remediation-*` annotations. A **restart mark** asks
  for a restart; a **replacement mark** (`remediation-action: Remediate`)
  asks for a replacement.
- A **remediation object** is what a MachineHealthCheck creates from its
  remediation template, for the infrastructure provider to act on.
- NVSentinel **releases** a node when it hands it over through a request.
  The operator's `Machine released from remediation` and `would release
  Machine from remediation` log lines and its `RemediationReleased` Event
  mean something else: it removed, or would remove, a restart mark.

## Read

The operator runs in the management cluster and reaches every workload
cluster through the kubeconfig Secret Cluster API keeps for it. Per cluster
it reads one of two sources, chosen on every poll from the resources the
cluster serves:

- **Requests**, where the cluster serves `ExternalRemediationRequest`. A
  request carries the full health event, records that NVSentinel has
  released the node, and takes a completion status back. The resource comes
  with NVSentinel's janitor, which itself handles every action that
  NVSentinel's fault-remediation maps to the janitor's own objects, so in
  such a cluster the operator reads only requests and leaves node conditions
  alone. It watches the requests, so a request is read as soon as NVSentinel
  releases its node.
- **Node conditions**, published by NVSentinel's platform connector, whose
  messages carry `ErrorCode:…`, `GPU_UUID:…` and `Recommended Action=…`. They
  are read where NVSentinel only detects faults. A message cut at the
  connector's length limit has lost its last events, and the decision for
  it is flagged as truncated.

Two cases are read but not acted on, so that two systems never remediate the
same node:

- A cluster with the janitor but without `ExternalRemediationRequest`, as
  before NVSentinel v1.10, is in `ReportOnly` mode: its node conditions are
  reported.
- With NVSentinel v1.10 to v1.12 the resource is served, but the janitor
  never releases a node to a request, so the operator neither acts nor
  reports anything in such a cluster.

The operator logs the mode it chose for a cluster after every start and
whenever it changes; outside dry-run mode it also records a
`SignalSourceSelected` Event on the Cluster.
[Signal source](configuration.md#signal-source) says how a resource that is
added or removed later is noticed.

## Decode and decide

The recommended actions, error codes and GPU UUIDs are taken from the
signal. The actions are reduced to one decision with the
[decision table](configuration.md#decisions); when a signal carries several
actions, the most disruptive one wins.

NVSentinel's action names end in `_VM` or `_BM`, but the suffix does not say
what backs the node
([NVIDIA/NVSentinel#1661](https://github.com/NVIDIA/NVSentinel/issues/1661)),
so the table applies to every infrastructure provider. The default reports
everything a restart or a replacement is not known to fix. One reason is
that DCGM reports a false IMEX failure on topologies without a multi-node
NVLink domain
([NVIDIA/NVSentinel#1471](https://github.com/NVIDIA/NVSentinel/issues/1471)).
NVSentinel recommends neither a restart nor a replacement for it, and
following that recommendation keeps the false alarm from costing a node.

## Act

The node is mapped to its Machine through the Machine's `status.nodeRef`, and
the decision is expressed with Cluster API's own contracts:

- **Replace:** the operator sets `cluster.x-k8s.io/remediate-machine`. Every
  MachineHealthCheck that selects the Machine honors that annotation whatever
  its own checks are, and remediates the Machine: its owner replaces it, or,
  where the MachineHealthCheck has a remediation template, the provider's
  remediation runs first.
- **Restart:** the same annotation, set only when every MachineHealthCheck
  that selects the Machine has a remediation template. The MachineHealthCheck
  creates the remediation object, and the provider restarts the Machine.

The Machine is not marked when:

- it is being deleted, or already marked;
- it is a control plane Machine, which the operator leaves to people;
- it is paused, or opted out with `cluster.x-k8s.io/skip-remediation`, so
  that a mark would take effect at an arbitrary time later;
- no MachineHealthCheck that is not paused selects it, so that a mark would
  do nothing.

The signal is reported with an Event instead, except for a Machine that is
being deleted or already marked, which is only logged. A request waits or is
declined; see [operations.md](operations.md#how-requests-are-answered).

### Why a mark and not a remediation object

The operator never creates the remediation object itself. A
MachineHealthCheck deletes the remediation object of every Machine it finds
healthy, and NVSentinel's signals are not among the conditions it checks, so
an object the operator created would be deleted again. Marking the Machine
leaves the remediation object, its lifecycle and the MachineHealthCheck's
unhealthy limits with Cluster API, and needs nothing from the provider beyond
the contract every remediation template already implements.

### How a restart ends

Cluster API never removes the `remediate-machine` annotation; while it is
there, the Machine stays unhealthy, and a provider that keeps restarting it
eventually gives up and has it replaced. So the operator removes its restart
mark once the restart is over:

- **Node condition:** when NVSentinel clears the condition, which it does
  after a reboot or once its check passes again.
- **Request:** NVSentinel no longer watches a node it has released, so the
  condition never clears. The operator records the node's boot ID with the
  restart mark. Once the node is Ready with a different boot ID, it answers
  the request with `True`, and then removes the mark.

The mark is also removed when the signal goes away before the restart: the
condition clears, or the request is deleted or answered elsewhere. A restart
answers the request it was asked for and no other; a request raised for the
node after it is back gets a restart of its own.

Before marking a Machine for a restart again, the operator waits until the
MachineHealthChecks have dropped what they kept for the previous mark, for
about a minute at most; a mark set too early would be counted against the
previous remediation object.

### Escalation

If a replacement is called for while a restart mark is on the Machine, the
operator turns the restart mark into a replacement mark, which it never
removes: a restart clears a signal read from the kernel log whether or not
the fault is gone, so the replacement has to outlive it.

That happens on a paused or opted-out Machine too. The pause or the opt-out
keeps Cluster API from acting on the mark either way, and the replacement
goes ahead once it is lifted. If the operator waited instead, a restart
already under way, or the one Cluster API starts as soon as the hold is
lifted, could clear the signal before the operator's next poll, and the
replacement would be lost. To keep such a Machine, remove the mark as well;
see [operations.md](operations.md#undoing-a-mark).

A restart that the restart fallback turns into a replacement is different:
on a paused or opted-out Machine the restart mark is not turned into a
replacement mark. Nothing new restarts the Machine while the hold lasts, so
its signal normally stays, and the fallback is decided again once the hold
is lifted, under the MachineHealthChecks that will then act. Should the
signal clear anyway, the restart is no longer called for, and the mark is
removed like any restart mark.

### The restart fallback

A restart that no remediation template can carry out is reported by default,
since marking the Machine would replace it. Platforms that prefer an
automatic recovery to a cheaper repair their provider cannot offer can have
it turned into a replacement instead; see
[Restart fallback](configuration.md#restart-fallback).

## Answer

When the signal came from a request, the operator writes the outcome to the
request's `ExternalRemediationComplete` condition: `True` once a restart is
over, which hands the node back to NVSentinel, and `False` at once when it
will not act. A replacement is not answered: the node is deleted with its
Machine, and since NVSentinel makes the node the request's owner, Kubernetes
deletes the request with it. The full table is in
[operations.md](operations.md#how-requests-are-answered).

The operator reads each request again from the workload cluster's API server
before it does anything about it, so a request answered a moment ago is
never acted on twice.

## Device quarantine

[Device quarantine](device-quarantine.md) is a separate controller that
watches Machines for the `nvsentinel.petasus.io/replace` request and nothing
else. It does not share the polling of the signals: a request is made by hand
and should be acted on at once, and its failures and retries have nothing to
do with those of a cluster's signals.

**Why `DeviceTaintRule`.** A rule is the standard way for an administrator to
taint a DRA device: the scheduler honours it whatever driver publishes the
device, so no driver has to take part, and it is an ordinary object that
`kubectl` lists and deletes. The rules are written as unstructured objects,
in `v1` where the cluster serves it and else in `v1beta2`: the module's
Kubernetes API types have no `v1` of them yet, and typed `v1beta2` alone
would break once that version is gone. The few fields written are the same
in both.

**One rule per device, named after it.** The name is derived from the
driver, pool and device, so a device is never tainted twice by the operator
and a request carried out again, after a restart of the operator for
instance, creates nothing new. The rule has no owner, because the device has
to stay out of scheduling after the Machine is gone, until someone repairs it
and deletes the rule. Hardware does not heal on its own, so nothing deletes
rules automatically.

**`NoSchedule`, not `NoExecute`.** The rule only keeps the device from being
allocated again; it evicts nobody. The Machine's own deletion drains the
node.

**The rules first, then the deletion.** The Machine is deleted only once
every rule has been read back. The rules do not show in the ResourceSlices,
so there is nothing else to wait for: the scheduler applies a rule as soon as
it sees it. The deletion carries the Machine's UID as a precondition, so a
Machine of the same name made since is never deleted in its place. Deleting
the Machine, rather than marking it, replaces it whether or not a
MachineHealthCheck selects it; a MachineSet has to own it, or nothing would
replace it.

**Which devices.** The operator follows the Machine to its
`KubevirtMachine`, the VM's `VirtualMachineInstance`, its virt-launcher pods
and their ResourceClaims, and keeps the devices of the configured drivers.
Driver names are the one standard field that tells devices apart in every
cluster; there is no standard way to tell a GPU from other devices by their
attributes, and one driver may publish several kinds. When more than one
device is left, the request has to name them: a device's name alone, which
is unique among the devices of one VM in practice, and is rejected rather
than guessed at when it is not.

**The devices of the running VM.** A VM that restarts is given its devices
anew, through a new virt-launcher pod and new claims, and may come back with
other devices than the faulty ones. So the devices are taken from a VM that
is running and not being deleted, a Machine marked for remediation, whose
VM may be restarting or about to, is rejected, and a request found
`replacing` after an interruption is not looked up again but goes straight
on to the deletion: the rules it needs are all in place.

**Guards and changes.** The guards of a mark apply, the mark itself
included. A rejection is final until the status is removed, so that a
Machine is never replaced at some later time nobody chose, once its Cluster
is unpaused for instance. Every write is conditional on the Machine being as
it was read: the status patches carry its resourceVersion, and the deletion
the resourceVersion of the last patch as well as the UID. A request edited
or withdrawn while it is carried out, or a Machine paused meanwhile, makes
the next write fail, and the request is evaluated again from the start. The
same keeps a cache that lags behind the last patch from acting a second
time.

**What cannot be checked.** On Kubernetes 1.36 the rules have effect only
if kube-scheduler runs with the `DRADeviceTaintRules` gate. Nothing in the
API shows whether it does, so the operator trusts the rule once it is
served.

## Provider neutrality

The operator imports Cluster API core only, and both of its actions are
contracts every infrastructure provider already speaks: the
`remediate-machine` annotation is consumed by Cluster API's own
MachineHealthCheck and MachineSet controllers, and a restart is the external
remediation template contract behind a MachineHealthCheck's `templateRef`.
What a provider has to offer for restarts is described in
[machinehealthchecks.md](machinehealthchecks.md#what-a-remediation-template-has-to-do).

Device quarantine is the one feature tied to a provider. Finding the devices
of a Machine depends on how the provider runs it; that is kept behind one
interface, implemented for Cluster API Provider KubeVirt, and read without
importing KubeVirt or the provider. The rules themselves are plain
Kubernetes.
