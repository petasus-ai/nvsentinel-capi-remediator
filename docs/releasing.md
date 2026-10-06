# Releasing

A release is a version tag, normally on `main`:

```
git tag v0.1.0 && git push origin v0.1.0
```

The `release` workflow then runs `make verify` and publishes, in this order:

1. the image, as `quay.io/edgestack/nvsentinel-capi-remediator:v0.1.0`, for
   linux/amd64 and linux/arm64, with `latest` moved to it when it is the
   newest release;
2. the chart, packaged as version `0.1.0` (the tag without its `v`) with the
   image above as its default, committed to the Helm repository
   `petasus-ai/edgestack-helm`, whose own workflow rebuilds the index;
3. a GitHub release with generated notes, carrying `install.yaml` (the
   kustomize manifests rendered with that image) and the chart package.

A tag with a suffix, such as `v0.1.0-rc.1`, is a pre-release: the GitHub
release is marked as one and `latest` does not move. Helm only selects such a
chart version when it is asked for with `--version` or `--devel`. A patch to
an earlier release line does not move `latest` either, and is not GitHub's
latest release.

Rules:

- **Never move or reuse a tag.** A chart version that is already published
  with other contents stops the workflow, and an image tag that is already
  published is not rebuilt.
- **Run it again after a failure.** Each step skips what an earlier run of
  the same tag already published, so a rerun finishes the release.
- **Do not create the release in the GitHub UI.** The workflow leaves a
  published release as it is, without the installer and the chart attached,
  and replaces a draft for the tag with its own.
- **Give the chart a few minutes.** The Helm repository is served from
  GitHub with a five-minute cache, and its index is rebuilt after the push.

The workflow uses the organization secrets `QUAY_USER_NAME` and
`QUAY_PASSWORD`, an account that can push to the image repository, and
`MY_TOKEN`, a token that can push to the Helm repository. Only the steps
that publish are given them; `make verify` runs in a job that has none.

The first push creates the image repository, which quay.io may well create
private. Before the chart goes out, the workflow checks that the image can be
pulled without credentials, and stops if it cannot: make the repository
public and run the workflow again. Expect that on the first release.

## The dev image

Every push to `main` that passes the `verify` workflow publishes the image
built from it as `quay.io/edgestack/nvsentinel-capi-remediator:dev`, for the
same platforms. It is `main` as it is now, for trying a change before it is
released, and nothing else is published with it: no chart, no installer and
no GitHub release. A release never moves `dev`, and a push to `main` never
moves `latest` or a version tag.

The tag moves with every push, so run it with the pull policy `Always`,
and read the commit it was built from off its
`org.opencontainers.image.revision` label:

```
helm upgrade nvsentinel-capi-remediator ... --reuse-values \
  --set image.tag=dev --set image.pullPolicy=Always
```

It uses the same two image secrets as a release, given only to the step
that logs in.
