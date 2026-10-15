<!--
Thank you for the pull request. CONTRIBUTING.md describes how changes are
made here; in short, every commit is signed off (git commit -s) and the
pull request title becomes the subject of the squashed commit.
-->

#### What this changes and why

#### Related issue

<!-- "Fixes #123", or the upstream NVSentinel or Cluster API issue it follows. -->

#### How it was checked

<!--
make verify runs on every pull request. Say what else you did: a test you
added, a dry run in a management cluster, a fault you injected. For a run
against real clusters, name the infrastructure and control plane providers,
the Cluster API and NVSentinel versions, and the signal source
(ExternalRemediationRequest or node condition).
-->

#### Checklist

- [ ] Every commit is signed off by its author.
- [ ] Tests cover the change, or the description says why they cannot.
- [ ] README.md and docs/ describe the behavior as it is after this change.
- [ ] A new or changed flag is reflected in the chart and in config/.
