# Security policy

## Reporting a vulnerability

Please do not report a vulnerability in a public issue. Report it privately
through [GitHub's private vulnerability reporting](https://github.com/petasus-ai/nvsentinel-capi-remediator/security/advisories/new),
or by email to [changju.gim@sk.com](mailto:changju.gim@sk.com).

Describe what an attacker could do, the versions affected and how to
reproduce it. You will be told when the report has been read, and what is
done about it.

## Supported versions

Fixes are released as a patch of the newest minor version, from its release
branch (see [docs/releasing.md](docs/releasing.md)). Older minor versions are
not patched.

## Scope

The operator runs in the management cluster. It reads the kubeconfig Secrets
Cluster API keeps for the workload clusters, which grant whatever access
those kubeconfigs carry, and it annotates Machines, which can make Cluster
API restart or replace them. A way to make the operator mark a Machine it
should not, or to use its access for anything else, is in scope. Problems in
NVSentinel or Cluster API themselves belong to those projects.
