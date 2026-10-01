# nvsentinel-capi-remediator

Bridges [NVSentinel](https://github.com/NVIDIA/NVSentinel) health signals to
[Cluster API](https://cluster-api.sigs.k8s.io/) machine remediation.

NVSentinel detects GPU and fabric faults inside a workload cluster and knows the
recommended action for each of them. The Cluster API `Machine` that could act on
a fault lives in the management cluster. Nothing upstream connects the two: this
operator reads NVSentinel's signals from each workload cluster, decides what the
management cluster should do, and hands that decision to Cluster API through
contracts every infrastructure provider already implements.

## Status

The manager runs in a management cluster and, in its default dry-run mode,
logs the decision it would take for every NVSentinel signal found in the
workload clusters: `ExternalRemediationRequest` objects where the cluster
serves them, node conditions otherwise. With `--dry-run=false` it asks
Cluster API to remediate the Machines whose signal maps to a replacement or,
where the cluster's MachineHealthChecks remediate through a template, to a
restart, and records what it did as Events on them. A Machine marked for a
restart is released again once the restart is over, and a request is
answered with the outcome. The design below describes the whole of what is
being built.

## Design

1. **Read.** The operator runs in the management cluster and reaches every
   workload cluster through the kubeconfig Cluster API maintains for it. In
   each cluster it reads NVSentinel's signals from one of two sources, chosen
   on every poll by what the cluster serves:
   - `ExternalRemediationRequest` objects, NVSentinel's protocol for handing a
     node to an external remediation system. They carry the full health event,
     mark the node as released by NVSentinel, and take a completion status back.
     The resource ships with NVSentinel's janitor, which remediates by itself
     every recommended action not routed to a request, so where it is served
     the requests are the only source and node conditions are left alone.
   - Node conditions published by NVSentinel's Kubernetes platform connector,
     whose message carries `ErrorCode:…`, `GPU_UUID:…` and
     `Recommended Action=…`. Used when a cluster runs NVSentinel in
     detection-only mode. Messages that hit the connector's length limit are
     flagged, since those have lost trailing events.

   A cluster with the janitor but without `ExternalRemediationRequest`, as in
   NVSentinel v0.5 to v1.9, is observed but not acted on, so that two systems
   never remediate the same node. With NVSentinel v1.10 to v1.13 the resource
   is served but the janitor never releases a node to a request, so nothing
   there is reported either. The source chosen for a cluster is recorded
   as an Event on the Cluster when it is first chosen after the operator
   starts and whenever it changes, except in dry run. A resource installed
   later is noticed on the next poll; a CRD that is removed is only noticed
   once the operator reconnects to the cluster, and the CRDs Helm leaves
   behind when NVSentinel's janitor is disabled keep the cluster in the mode
   they imply.
2. **Decode.** The recommended actions, error codes and GPU UUIDs are recovered
   from the signal.
3. **Decide.** The actions are reduced to one platform decision. When a message
   carries several actions the most disruptive one wins.
4. **Act.** The node is mapped back to its `Machine` through `status.nodeRef`
   and the decision is expressed with Cluster API primitives only. When the
   signal came from an `ExternalRemediationRequest`, the outcome is written back
   to its `ExternalRemediationComplete` condition: `True` once a restart is
   over, which hands the node back to NVSentinel, and `False` straight away
   when the operator will not act, because the decision is to report, no
   Machine owns the node, it belongs to the control plane, it opted out of
   remediation, or no MachineHealthCheck can carry out the decision, not even
   once the paused ones are unpaused. A replacement is not answered: the node
   is deleted with its Machine, and Kubernetes deletes the request with it,
   since NVSentinel makes the node its owner. A request waits while its
   Machine is paused or already being remediated, and while the checks that
   could act on it are paused. Requests are read again from the API server
   before anything is done about them, so one answered a moment ago is never
   acted on twice.

| NVSentinel recommended action | Decision | Cluster API action |
|---|---|---|
| `REPLACE_VM` | Replace | Set the `cluster.x-k8s.io/remediate-machine` annotation on the Machine. Every MachineHealthCheck selecting it honours the annotation regardless of its configured checks and applies its remediation: the owning MachineSet replaces the Machine, or, where the check has a remediation template, the provider's remediation runs first. |
| `RESTART_VM`, `RESTART_BM` | Restart | The same annotation, set only when every MachineHealthCheck selecting the Machine remediates through a template (`spec.remediation.templateRef`). The check creates the request from its template and the infrastructure provider's remediation controller performs the restart. |
| `CONTACT_SUPPORT`, `COMPONENT_RESET`, `RUN_FIELDDIAG`, `NONE`, … | Report | Log and emit an Event. These never delete a node: DCGM reports a false IMEX failure on topologies without a multi-node NVLink domain (NVIDIA/NVSentinel#1471), and honouring the recommended action is what keeps that from costing a node. |

NVSentinel's `_VM` and `_BM` suffixes are lifecycle verbs rather than a
statement about what backs the node (NVIDIA/NVSentinel#1661), so the table
applies unchanged whatever the infrastructure provider is.

## Provider neutrality

The operator imports Cluster API core only. Both actions above are contracts
that every infrastructure provider already speaks:

- The `remediate-machine` annotation is consumed by Cluster API's own
  MachineHealthCheck and MachineSet controllers.
- A restart is the external remediation template contract that
  MachineHealthCheck uses for `templateRef`. The operator never creates the
  request itself: a MachineHealthCheck deletes the request named after every
  Machine it finds healthy, and NVSentinel's signals are not among its checks.
  Marking the Machine leaves the request, its lifecycle and the check's
  unhealthy-count limits with Cluster API. Cluster API never removes the
  annotation, so the operator removes the ones it set for a restart once
  the restart is over. For a node condition that is when NVSentinel lowers
  it, which it does after a reboot or once its check passes again. A node
  released to an `ExternalRemediationRequest` is no longer watched by
  NVSentinel, so there a new boot ID on a Ready node ends the restart: the
  operator records the node's boot ID with every restart mark, answers the
  request once the node reports another, and then removes the mark. Until
  then the Machine stays unhealthy and a provider that gives up on
  restarting it has it replaced. A replacement called for while the restart
  is pending turns the mark into a replacement mark, which is never removed.

A Machine that no MachineHealthCheck selects, or only paused ones, is not
marked, since the annotation would do nothing, or act at some arbitrary time
after an unpause; the signal is reported with an Event instead. A restart
that no template can carry out, because no active check selects the Machine
or one without a template does, follows the restart fallback:

- `report` (the default) reports it the same way, since marking the Machine
  would replace it.
- `replace` marks it for a replacement instead, for operators who prefer an
  automatic recovery over a cheaper repair their provider cannot offer. That
  mark is never removed, and an earlier restart mark on the Machine becomes a
  replacement mark. It still needs a MachineHealthCheck to act on it.

The fallback is set with `--restart-fallback` and overridden per cluster with
the `nvsentinel.petasus.io/restart-fallback` annotation on the Cluster (under
the operator's annotation prefix), which takes effect on the next poll. An
invalid annotation is ignored, and the Events of the signals it affects say
so.

## Planned safeguards

- Dry-run is the default. Decisions are logged and nothing is written until
  remediation is enabled explicitly.
- Control plane Machines are never remediated automatically.
- A Machine that is already marked for remediation, already being deleted,
  paused, or opted out with `cluster.x-k8s.io/skip-remediation` is left alone.
- Every action leaves a Kubernetes Event on the Machine, because the Machine
  and its annotations disappear once remediation succeeds.

## Roadmap

Done: signal decoder and decision table with tests built from real
NVSentinel messages; the controller with workload cluster polling, Machine
mapping and the replacement path, dry-run by default; the container image,
kustomize deployment and Helm chart; restarts through the MachineHealthChecks' remediation
templates, released again once the restart is over, with a configurable
fallback for restarts no template can carry out; the
`ExternalRemediationRequest` source, chosen per cluster, with the outcome
reported back to NVSentinel.

Next:

- Configurable decision table, integration tests.
- A release workflow that publishes the image and the chart.

## Development

```
make verify                        # gofmt, go.mod, license headers, RBAC, kustomize and chart manifests, vet, tests
make docker-build                  # image for the local Docker daemon, tagged IMG
make docker-buildx IMG=<image>     # linux/amd64 and linux/arm64 image, pushed to IMG
make deploy IMG=<image>            # render config/default with that image and apply it to the current kubectl context
make build-installer IMG=<image>   # the same rendering, written to dist/install.yaml
make undeploy
```

No image is published yet, so `IMG` has to name one you pushed with
`make docker-buildx`. The manager is deployed to the
`nvsentinel-capi-remediator-system` namespace with `--dry-run=true`. Once its
log shows the decisions you expect, change the argument to `--dry-run=false`
to let it act. `make deploy` re-applies the checked-in manifests, so make that
change in a kustomize overlay of `config/default` if it has to survive a
redeploy.

The same manager can be installed with the Helm chart in
`charts/nvsentinel-capi-remediator`, into a namespace of your choice:

```
helm install nvsentinel-capi-remediator charts/nvsentinel-capi-remediator \
  --namespace <namespace> --create-namespace \
  --set image.repository=<repository> --set image.tag=<tag>
```

It starts in dry-run as well; `--set dryRun=false` lets it act. The other
settings are described in the chart's `values.yaml`. Install it one way or
the other: a kustomize and a Helm install in different namespaces each hold
their own lease, so both would act.

## Contributing

Contributions are welcome. Every commit must carry a Developer Certificate of
Origin sign-off (`git commit -s`); see [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
