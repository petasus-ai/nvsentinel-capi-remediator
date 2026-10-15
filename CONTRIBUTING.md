# Contributing

Thank you for your interest in contributing. Everyone taking part is
expected to follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## Where to start

Work that is open for anyone to pick up is tracked in issues labelled
[help wanted](https://github.com/petasus-ai/nvsentinel-capi-remediator/issues?q=is%3Aissue%20is%3Aopen%20label%3A%22help%20wanted%22).
Comment on one before you start, so that two people do not do the same work.

Running the operator in its default dry-run mode in your management cluster
and telling us which decisions it logs for real signals is a contribution in
itself. The operator speaks Cluster API core only, but it has run in few
environments so far, and every new one shows whether that holds.

## Pull requests

Open an issue first for anything larger than a small fix, so that the
approach is agreed on before the work is done. A pull request needs
`make verify` to pass, which CI runs together with a multi-platform image
build and the sign-off check below, and a review by a code owner. Pull
requests are squash-merged: the pull request title becomes the subject of
the commit, so give it the form described under commit messages.

## Developer Certificate of Origin

This project uses the [Developer Certificate of Origin](https://developercertificate.org/)
(DCO) instead of a contributor license agreement. Every commit must be signed
off, which certifies that you wrote the change or otherwise have the right to
submit it under the project's license:

```
git commit -s
```

This adds a `Signed-off-by:` trailer with the name and email from your Git
configuration, which must be those the commit is authored with. Pull
requests whose commits lack the trailer cannot be merged. `make verify-dco`
runs the same check on your branch against `origin/main` before you push.

## License headers

Every Go file starts with the header in `hack/boilerplate.go.txt`.
`make verify-boilerplate` checks this, and `make verify` runs it together with
`gofmt`, `go vet` and the tests.

## Commit messages

Start the subject with the kind of change and follow it with a short
imperative phrase, as in `Fix: answer a restart to the request that asked
for it`: `Feat:` for new behavior, `Fix:` for a bug, `Docs:`, `Test:`,
`Refactor:` for a change that keeps the behavior, `Style:` for formatting
and `Chore:` for the build, CI and dependencies. Explain *why* in the body.
Reference upstream issues (NVSentinel, Cluster API, providers) by full URL or
`owner/repo#number` so they stay resolvable outside GitHub.

## Reporting a fault mapping problem

If NVSentinel reports a fault that this project maps to the wrong decision,
please open a fault mapping report with the verbatim node condition message
or request. The decision table is covered by tests built from real messages,
and yours can become one.

## Reporting a security problem

Do not open a public issue. [SECURITY.md](SECURITY.md) describes how to
report it privately.
