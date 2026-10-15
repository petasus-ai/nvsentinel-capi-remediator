# Releasing

For maintainers: how a release is cut and what it publishes.

A release is a version tag on the release branch of its minor version:
`v0.2.1` is cut from `release-0.2`.

Before you tag a release, commit `version` and `appVersion` in
`charts/nvsentinel-capi-remediator/Chart.yaml` set to the version you are
about to tag: on `main` while the release branch is fast-forwarded to it, and
on the release branch itself once that is kept up by cherry-picks. The
workflow stamps the same values into the package; committing them keeps a
checkout of the tag installing its own image. Leave them as they are for a
pre-release such as `v0.3.0-rc.1`, so that `main` keeps naming a release, and
raise them on `main` too whenever it still names an older release.

The first release of a minor version cuts the branch from `main`:

```sh
git switch -c release-0.3 main && git push origin release-0.3
git tag v0.3.0 && git push origin v0.3.0
```

Every later release of that branch raises the patch version. While `main` has
nothing that waits for the next minor version, fast-forward the branch to
`main`; after that, cherry-pick what the release needs:

```sh
git switch release-0.3 && git merge --ff-only main && git push origin release-0.3
git tag v0.3.1 && git push origin v0.3.1
```

The `release` workflow then runs `make verify` and publishes, in this order:

1. the image, as `quay.io/edgestack/nvsentinel-capi-remediator:v0.3.1`, for
   linux/amd64 and linux/arm64, with `latest` moved to it when it is the
   newest release;
2. the chart, packaged as version `0.3.1` (the tag without its `v`) with the
   image above as its default, committed to the Helm repository
   `petasus-ai/edgestack-helm`, whose own workflow rebuilds the index;
3. a GitHub release with generated notes, carrying `install.yaml` (the
   kustomize manifests rendered with that image) and the chart package.

A tag with a suffix, such as `v0.3.0-rc.1`, is a pre-release: the GitHub
release is marked as one and `latest` does not move. Helm only selects such a
chart version when it is asked for with `--version` or `--devel`. A patch to
an earlier release line does not move `latest` either, and is not GitHub's
latest release.

After the release, write in its GitHub release what changes for users: new
or changed flags and chart values, and changed behavior.

## Rules

- **Tag the release branch.** The workflow stops before it publishes
  anything if the tagged commit is not on the release branch of its minor
  version, or if that branch does not exist. A pre-release such as
  `v0.3.0-rc.1` is cut from `release-0.3` like the release it precedes.
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

## Secrets

The workflow uses the organization secrets `QUAY_USER_NAME` and
`QUAY_PASSWORD`, an account that can push to the image repository, and
`MY_TOKEN`, a token that can push to the Helm repository. Only the steps
that publish are given them; `make verify` runs in a job that has none.

A new image repository may be created private by its first push. Before the
chart goes out, the workflow checks that the image can be pulled without
credentials, and stops if it cannot: make the repository public and run the
workflow again.

## The dev image

Every push to `main` that passes the `verify` workflow publishes the image
built from it as `quay.io/edgestack/nvsentinel-capi-remediator:dev`, for the
same platforms. When two pushes finish close together, only the one that is
still the head of `main` publishes. Nothing else is published with it: no
chart, no installer and no GitHub release. A release never moves `dev`, and
a push to `main` never moves `latest` or a version tag. It uses the same two
image secrets as a release, given only to the step that logs in.
[operations.md](operations.md#installing-other-builds) shows how to run it.
