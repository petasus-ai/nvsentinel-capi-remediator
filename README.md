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

Nothing runs yet. This repository holds the project scaffolding: license,
contribution rules and build targets. The design below describes what is being
built.

## Design

1. **Read.** The operator runs in the management cluster and reaches every
   workload cluster through the kubeconfig Cluster API maintains for it. In each cluster it reads
   NVSentinel's signals from one of two sources, chosen per cluster by what the
   cluster serves:
   - `ExternalRemediationRequest` objects, NVSentinel's protocol for handing a
     node to an external remediation system. They carry the full health event,
     mark the node as released by NVSentinel, and take a completion status back.
     Preferred whenever the CRD is present.
   - Node conditions published by NVSentinel's Kubernetes platform connector,
     whose message carries `ErrorCode:…`, `GPU_UUID:…` and
     `Recommended Action=…`. Used when a cluster runs NVSentinel in
     detection-only mode. Messages that hit the connector's length limit are
     flagged, since those have lost trailing events.

   A cluster where NVSentinel performs its own remediation, and no
   `ExternalRemediationRequest` is routed to this operator, is observed but not
   acted on, so that two systems never remediate the same node.
2. **Decode.** The recommended actions, error codes and GPU UUIDs are recovered
   from the signal.
3. **Decide.** The actions are reduced to one platform decision. When a message
   carries several actions the most disruptive one wins.
4. **Act.** The node is mapped back to its `Machine` through `status.nodeRef`
   and the decision is expressed with Cluster API primitives only. When the
   signal came from an `ExternalRemediationRequest`, the outcome is written back
   to it so NVSentinel can take the node back.

| NVSentinel recommended action | Decision | Cluster API action |
|---|---|---|
| `REPLACE_VM` | Replace | Set the `cluster.x-k8s.io/remediate-machine` annotation on the Machine. MachineHealthCheck honours it regardless of its configured checks and the owning MachineSet replaces the Machine. |
| `RESTART_VM`, `RESTART_BM` | Restart | Clone the cluster's external remediation template (`MachineHealthCheck.spec.remediation.templateRef`) with an owner reference to the Machine, exactly as MachineHealthCheck does. The infrastructure provider's remediation controller performs the restart. |
| `CONTACT_SUPPORT`, `COMPONENT_RESET`, `RUN_FIELDDIAG`, `NONE`, … | Report | Log and emit an Event. These never delete a node: DCGM reports a false IMEX failure on topologies without a multi-node NVLink domain (NVIDIA/NVSentinel#1471), and honouring the recommended action is what keeps that from costing a node. |

NVSentinel's `_VM` and `_BM` suffixes are lifecycle verbs rather than a
statement about what backs the node (NVIDIA/NVSentinel#1661), so the table
applies unchanged whatever the infrastructure provider is.

## Provider neutrality

The operator imports Cluster API core only. Both actions above are contracts
that every infrastructure provider already speaks:

- The `remediate-machine` annotation is consumed by Cluster API's own
  MachineHealthCheck and MachineSet controllers.
- The external remediation template contract is what MachineHealthCheck uses
  for `templateRef`. A provider's remediation controller resolves a request
  through its owner reference to the Machine, so a request created outside
  MachineHealthCheck is handled the same way as one MachineHealthCheck created.

When a cluster has no remediation template, a restart signal falls back to
one of two configurable behaviours: report it with an Event (the default), or
escalate it to a replace for operators who prefer an automatic recovery over a
cheaper repair that their provider cannot offer.

## Planned safeguards

- Dry-run is the default. Decisions are logged and nothing is written until
  remediation is enabled explicitly.
- Control plane Machines are never remediated automatically.
- A Machine that is already marked for remediation, or already being deleted,
  is left alone.
- Every action leaves a Kubernetes Event on the Machine, because the Machine
  and its annotations disappear once remediation succeeds.

## Roadmap

- Signal decoder and decision table, with tests built from real NVSentinel
  messages.
- Controller: workload cluster polling, Machine mapping and the annotation
  path.
- Restart path through external remediation templates, with per-cluster
  concurrency limits and cleanup of the remediation request once the node
  recovers.
- Configurable decision table.
- gRPC sink connector receiver, replacing node condition parsing with
  NVSentinel's lossless `HealthEvent`.

## Development

```
make verify   # gofmt, license headers, go vet, tests
```

## Contributing

Contributions are welcome. Every commit must carry a Developer Certificate of
Origin sign-off (`git commit -s`); see [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
