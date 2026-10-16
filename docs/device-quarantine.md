# Device quarantine

When a GPU fault survives a restart, the next step is to replace the
Machine. Deleting the Machine is not enough on its own: the faulty device is
free again once the old VM is gone, and the replacement can be given the very
same device. With device quarantine on, you ask the operator to replace the
Machine instead. It takes the devices of the Machine's VM out of scheduling
with Kubernetes `DeviceTaintRule`s first, and only then deletes the Machine
for its MachineSet to replace.

The feature is off by default and separate from the rest of the operator: it
acts only on your request, never on an NVSentinel signal.

## Requirements

- **Cluster API Provider KubeVirt.** The Machine's infrastructure is a
  `KubevirtMachine` whose VM runs in the management cluster. Machines whose
  VMs run in an external infrastructure cluster (`infraClusterSecretRef`) are
  rejected.
- **DRA devices handed to the VM by DRA drivers in the management cluster**,
  through the ResourceClaims of the VM's virt-launcher pod. Those are the
  devices the operator finds and quarantines.
- **`DeviceTaintRule` in the management cluster.** Kubernetes 1.37 serves it
  in `resource.k8s.io/v1` by default. Kubernetes 1.36 serves it in `v1beta2`
  only when you turn it on:
  - `--feature-gates=DRADeviceTaintRules=true` on kube-apiserver,
    kube-scheduler and kube-controller-manager. The scheduler is the one
    that applies the rules; with the gate on the API server alone, rules are
    accepted and do nothing. The operator cannot tell the difference, so
    check it once: a rule on a free device must leave a pod that asks for
    that device pending.
  - `--runtime-config=resource.k8s.io/v1beta2/devicetaintrules=true` on
    kube-apiserver.
- **A MachineSet that owns the Machine**, so that something replaces it. A
  Machine of a MachineDeployment qualifies.

## Turning it on

With the chart, list the DRA drivers whose devices a request quarantines:

```yaml
deviceQuarantine:
  enabled: true
  drivers:
  - gpu.nvidia.com
```

This adds the `--device-quarantine` and `--quarantine-drivers` flags and a
ClusterRole with the permissions below. Dry-run mode applies as everywhere
else: until `dryRun` is `false`, a request is only logged.

The manifests (`config/default`, and the release's `install.yaml` rendered
from it) leave device quarantine out entirely, role included. To turn it on
with kustomize, add the `config/components/device-quarantine` component,
which creates the role and binds it, to an overlay of `config/default`, and
add the flags. Pin the image too: `config/default` names `latest`.

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- ../default
components:
- ../components/device-quarantine
images:
- name: quay.io/edgestack/nvsentinel-capi-remediator
  newTag: <release>
patches:
- target:
    kind: Deployment
    name: nvsentinel-capi-remediator-controller-manager
  patch: |
    - op: add
      path: /spec/template/spec/containers/0/args/-
      value: --device-quarantine
    - op: add
      path: /spec/template/spec/containers/0/args/-
      value: --quarantine-drivers=gpu.nvidia.com
```

An install from `install.yaml` has nothing to add a component to. Create the
role from the release's `config/components/device-quarantine/role.yaml` under
the prefixed name, bind it, and add the flags to the Deployment:

```sh
curl -fsSL https://raw.githubusercontent.com/petasus-ai/nvsentinel-capi-remediator/<release>/config/components/device-quarantine/role.yaml |
  sed 's/name: device-quarantine-role/name: nvsentinel-capi-remediator-device-quarantine-role/' | kubectl apply -f -
kubectl create clusterrolebinding nvsentinel-capi-remediator-device-quarantine-rolebinding \
  --clusterrole=nvsentinel-capi-remediator-device-quarantine-role \
  --serviceaccount=nvsentinel-capi-remediator-system:nvsentinel-capi-remediator-controller-manager
kubectl -n nvsentinel-capi-remediator-system patch deployment nvsentinel-capi-remediator-controller-manager --type json -p '[
  {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--device-quarantine"},
  {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--quarantine-drivers=gpu.nvidia.com"}]'
```

The drivers keep the request to the devices you mean. A VM may hold devices
of several drivers, a GPU and a network function for instance, and only those
of the listed drivers are quarantined.

## Asking for a replacement

The devices quarantined are those the Machine's VM holds when the request is
carried out. A VM that restarts is given its devices anew, and may come back
with other ones, so ask while the fault shows on the running VM, not while a
restart is under way: a Machine marked for remediation, or whose VM is not
running, is rejected. Check the devices the request names against the
fault, which NVSentinel reports with the GPU's PCI address and UUID.

Annotate the Machine:

```sh
kubectl -n <namespace> annotate machines.cluster.x-k8s.io <machine> \
  nvsentinel.petasus.io/replace=quarantine
```

That is enough when the VM holds one device of the configured drivers. When
it holds more, name the faulty ones, by the device name their ResourceSlice
gives them:

```sh
kubectl -n <namespace> annotate machines.cluster.x-k8s.io <machine> \
  nvsentinel.petasus.io/replace=quarantine:<device>[,<device>]
```

A request that names no device while there are several is rejected with a
`QuarantineRejected` Event that lists them. They are also in the allocation
of the claims of the VM's virt-launcher pod: those its spec names, and those
made for it from a template, which its status names. The VM has the name of
the `KubevirtMachine`, and lives in the namespace of its template or else in
the Machine's:

```sh
uid=$(kubectl -n <vm-namespace> get vmi <vm> -o jsonpath='{.metadata.uid}')
kubectl -n <vm-namespace> get pods -l kubevirt.io/created-by="$uid" -o jsonpath='{range .items[*]}
  {range .spec.resourceClaims[*]}{.resourceClaimName}{"\n"}{end}
  {range .status.resourceClaimStatuses[*]}{.resourceClaimName}{"\n"}{end}{end}' |
  xargs -n1 | sort -u | xargs -I{} kubectl -n <vm-namespace> get resourceclaim {} \
  -o jsonpath='{range .status.allocation.devices.results[*]}{.driver} {.pool} {.device}{"\n"}{end}'
```

The operator then:

1. checks the request (see [Rejections](#rejections)), finds the devices
   allocated to the claims of the VM's virt-launcher pod and picks those to
   quarantine;
2. sets `nvsentinel.petasus.io/replace-status: quarantining` and creates a
   rule for each device;
3. reads the rules back, sets `replace-status: replacing` and records a
   `DevicesQuarantined` Event that names the rules;
4. deletes the Machine and records `MachineReplaced`.

Cluster API drains the node and deletes the VM, and the MachineSet creates a
new Machine. The scheduler applies a rule as soon as it exists, so the new
VM's claims are allocated other devices. If no other device is free, the new
VM stays pending until one is.

A request that stops halfway, because the operator restarted for instance,
carries on from its status: the rules have fixed names, so making them again
creates nothing new, and a request found `replacing` goes straight on to the
deletion without looking the devices up again, as long as the rules made for
the Machine still cover it. A request edited after its rules were made, or a
status set by hand, is carried out again from the start.

Each write is conditional on the Machine being as the operator read it. If
the Machine changes meanwhile, because the request is edited or withdrawn
or the Machine is paused, the operator starts over and the Machine as it is
then decides.

## The rules

Each quarantined device gets one cluster-scoped rule:

```yaml
apiVersion: resource.k8s.io/v1   # v1beta2 on Kubernetes 1.36
kind: DeviceTaintRule
metadata:
  name: quarantine-<16 hex digits derived from the driver, pool and device>
  labels:
    app.kubernetes.io/managed-by: nvsentinel-capi-remediator
    nvsentinel.petasus.io/machine: <machine>
    nvsentinel.petasus.io/machine-namespace: <namespace>
    nvsentinel.petasus.io/cluster: <cluster>
  annotations:
    nvsentinel.petasus.io/machine: <namespace>/<machine>
    nvsentinel.petasus.io/device: <driver>/<pool>/<device>
    nvsentinel.petasus.io/requested-at: <time>
spec:
  deviceSelector:
    driver: <driver>
    pool: <pool>
    device: <device>
  taint:
    key: nvsentinel.petasus.io/quarantined
    value: "true"
    effect: NoSchedule
```

`NoSchedule` keeps the device from being allocated again and evicts nobody:
whatever still uses it is left alone. A label value too long for a label is
shortened and ends in a digest; the annotation has the whole name.

A rule has no owner and outlives the Machine. A device that already has its
rule keeps it, with the labels of the Machine it was first quarantined for.

To list them:

```sh
kubectl get devicetaintrules -l app.kubernetes.io/managed-by=nvsentinel-capi-remediator \
  -L nvsentinel.petasus.io/cluster,nvsentinel.petasus.io/machine
```

## Releasing a device

Delete its rule once the device is repaired or replaced:

```sh
kubectl delete devicetaintrule <rule>
kubectl delete devicetaintrule \
  -l nvsentinel.petasus.io/machine=<machine>,nvsentinel.petasus.io/machine-namespace=<namespace>
```

A Machine name longer than 63 characters is shortened in the label; find its
rules by the `nvsentinel.petasus.io/machine` annotation instead.

The device can be allocated again right away. The operator never deletes a
rule.

## Rejections

A request that cannot be carried out as it stands is rejected: the status is
set to `rejected: <reason>`, a `QuarantineRejected` Event explains it, and
nothing else changes. A rejected request stays rejected. To try again, change
what made it fail and remove the status, in one command if you like:

```sh
kubectl -n <namespace> annotate machines.cluster.x-k8s.io <machine> --overwrite \
  nvsentinel.petasus.io/replace=quarantine:<device> nvsentinel.petasus.io/replace-status-
```

| Reason | Meaning |
|---|---|
| `InvalidRequest` | The value is neither `quarantine` nor `quarantine:` followed by device names. |
| `ControlPlaneMachine` | The Machine is a control plane member. |
| `AlreadyMarkedForRemediation` | The Machine carries `cluster.x-k8s.io/remediate-machine`: a restart or a replacement is under way, and the VM may come back with other devices. Ask again once the mark is gone, or [undo the mark](operations.md#undoing-a-mark). |
| `MachinePaused` | The Machine carries `cluster.x-k8s.io/paused`. |
| `ClusterPaused` | The Cluster is paused. |
| `MachineOptedOutOfRemediation` | The Machine carries `cluster.x-k8s.io/skip-remediation`. |
| `NotReplaceable` | No MachineSet owns the Machine, so nothing would replace it. |
| `NotKubeVirt` | The Machine's infrastructure is not a `KubevirtMachine`, or the cluster does not serve KubeVirt's kinds. |
| `ExternalInfraCluster` | The VM runs in an external infrastructure cluster. |
| `NoDevices` | The VM holds no device of the configured drivers, is not running or is being deleted, or the `KubevirtMachine` does not exist. |
| `AmbiguousDevices` | The VM holds several devices of the configured drivers and the request names none, or a name fits more than one of them. |
| `UnknownDevice` | A name in the request is not among the VM's devices of the configured drivers. |
| `DeviceTaintRuleUnavailable` | The cluster serves no `DeviceTaintRule` in `v1` or `v1beta2`. |
| `DeviceTaintRuleConflict` | A rule of the name the device's rule would have exists, but does not keep that device from being allocated. |
| `Forbidden` | The operator may not read or write what the request needs. |

A request on a Machine that is being deleted, or whose Cluster the operator
does not watch, is left alone without a status.

Rules made before a request was rejected, when it lists several devices for
instance, stay; delete them as above if you do not mean to try again.

Removing the request removes its status too. Rules already made stay.

## Permissions

The device quarantine ClusterRole adds the permissions below. The chart
names it `<release fullname>-device-quarantine`; the kustomize component
names it `nvsentinel-capi-remediator-device-quarantine-role`.

| It reads or writes | Why |
|---|---|
| `Machine` (delete) | replacing the Machine |
| `KubevirtMachine`, `KubevirtCluster` (get) | the VM's name and namespace, and whether it runs in an external cluster |
| `VirtualMachineInstance` (get) | the VM, and its active pods |
| `Pod` (list) | the VM's virt-launcher pods, by the `kubevirt.io/created-by` label |
| `ResourceClaim` (get) | the devices allocated to the pods |
| `DeviceTaintRule` (get, list, create) | the rules, and those made for a Machine when its request is picked up again |

The pods, claims and rules are read straight from the API server, not
cached, so the operator holds none of them in memory.
