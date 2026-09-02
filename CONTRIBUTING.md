# Contributing

Thank you for your interest in contributing.

## Developer Certificate of Origin

This project uses the [Developer Certificate of Origin](https://developercertificate.org/)
(DCO) instead of a contributor license agreement. Every commit must be signed
off, which certifies that you wrote the change or otherwise have the right to
submit it under the project's license:

```
git commit -s
```

This adds a `Signed-off-by:` trailer with the name and email from your Git
configuration. Pull requests whose commits lack the trailer cannot be merged.

## License headers

Every Go file starts with the header in `hack/boilerplate.go.txt`.
`make verify-boilerplate` checks this, and `make verify` runs it together with
`gofmt`, `go vet` and the tests.

## Commit messages

Use a short imperative subject line and explain *why* in the body. Reference
upstream issues (NVSentinel, Cluster API, providers) by full URL or
`owner/repo#number` so they stay resolvable outside GitHub.

## Reporting a fault mapping problem

If NVSentinel reports a fault that this project maps to the wrong decision,
please include the verbatim node condition message. The decision table is
covered by tests built from real messages, and yours can become one.
