# Contributing

Thank you for your interest in contributing. Everyone taking part is
expected to follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## Where to start

Work that is open for anyone to pick up is tracked in issues labeled
[help wanted](https://github.com/petasus-ai/nvsentinel-capi-remediator/issues?q=is%3Aissue%20is%3Aopen%20label%3A%22help%20wanted%22).
Comment on one before you start, so that two people do not do the same work.

Running the operator in its default dry-run mode in your management cluster
and reporting the decisions it logs for real signals is a contribution in
itself. The operator speaks Cluster API core only, but it has run in few
environments so far, and each new one tests that claim. Report what you saw
in [#13](https://github.com/petasus-ai/nvsentinel-capi-remediator/issues/13),
or open an
[environment request](https://github.com/petasus-ai/nvsentinel-capi-remediator/issues/new?template=feature_request.yml).

## Development

| Target | What it does |
|---|---|
| `make verify` | The checks of CI's `make verify` job: gofmt, go.mod, license headers, generated manifests, kustomize and chart rendering, vet, unit and integration tests. |
| `make test-integration` | The manager against two real API servers, downloaded on first use. |
| `make verify-dco` | Every commit after `DCO_BASE` (default `origin/main`) is signed off by its author. |
| `make manifests` | Regenerate the RBAC in `config/rbac` and the chart's copies of it. |
| `make docker-build` | An image for the local Docker daemon, tagged `IMG`. |
| `make docker-buildx IMG=<image>` | A linux/amd64 and linux/arm64 image, pushed to `IMG`. |
| `make deploy IMG=<image>` | Render `config/default` with that image and apply it to the current kubectl context; `make undeploy` removes it. |
| `make build-installer IMG=<image>` | The same rendering, written to `dist/install.yaml`. |

[docs/operations.md](docs/operations.md#installing-other-builds) shows how to
run a build of your own, and [docs/design.md](docs/design.md) how the
operator works.

## Pull requests

Open an issue first for anything larger than a small fix, so that the
approach is agreed on before the work is done. A pull request needs
`make verify` to pass and a review by a code owner. CI runs `make verify`
together with a multi-platform image build and the sign-off check below.

## Commit messages

Pull requests are squash-merged, and the pull request title becomes the
commit subject, so give the title the same form. Start the subject with the
kind of change, followed by a short imperative phrase, as in
`Fix: answer a restart to the request that asked for it`:

- `Feat:` new behavior
- `Fix:` a bug
- `Docs:` documentation only
- `Test:` tests only
- `Refactor:` a change that keeps the behavior
- `Style:` formatting
- `Chore:` the build, CI and dependencies

Explain *why* in the body. Reference upstream issues (NVSentinel, Cluster
API, providers) by full URL or `owner/repo#number` so they stay resolvable
outside GitHub.

## Developer Certificate of Origin

This project uses the [Developer Certificate of Origin](https://developercertificate.org/)
(DCO) instead of a contributor license agreement. Every commit must be signed
off, which certifies that you wrote the change or otherwise have the right to
submit it under the project's license:

```sh
git commit -s
```

This adds a `Signed-off-by:` trailer with the name and email from your Git
configuration; its email must match the commit author's. Pull requests whose
commits lack the trailer cannot be merged; merge commits and commits by
GitHub Apps such as Dependabot are exempt. Run `make verify-dco` before you
push to check your branch against `origin/main` the same way.

## License headers

Every Go file starts with the header in `hack/boilerplate.go.txt`.
`make verify-boilerplate` checks this, and `make verify` runs it together with
the other checks.

## Reporting a fault mapping problem

If NVSentinel reports a fault that this project maps to the wrong decision,
please open a
[fault mapping report](https://github.com/petasus-ai/nvsentinel-capi-remediator/issues/new?template=fault_mapping.yml)
with the verbatim node condition message or request. The decision table is
covered by tests built from real messages, and yours can become one.

## Reporting a security problem

Do not open a public issue. [SECURITY.md](SECURITY.md) describes how to
report it privately.
