# Releasing

A release is a dispatch of [`release.yml`](../.github/workflows/release.yml) from main with a version. A dry run is the default and publishes nothing.

## What a release publishes

| Artifact | Where | Named by |
| --- | --- | --- |
| Controller image for amd64 and arm64, with provenance and an SBOM | `ghcr.io/azure/fiberd-k8s-controller` | Its digest, and one tag that is the exact version |
| Chart, with the image digest in its values | `oci://ghcr.io/azure/charts/fiberd-k8s-controller` | Its version, and its digest in the release notes |
| `install.yaml`, with the image by digest | Release asset | The release tag |
| `fiberd-k8s-controller-<version>.tgz` | Release asset | The release tag |
| `SHA256SUMS` over both files, with an attestation | Release asset | The release tag |

The image, the chart and the checksums each carry a GitHub attestation. No `latest` or `X.Y` tag exists, and no tag moves.

## Versions

- A version is `vMAJOR.MINOR.PATCH`, or that with `-alpha.N`, `-beta.N` or `-rc.N`.
- The image tag and the chart version are the version without its `v`.
- No file holds the version, so a release needs no version commit.
- A pre-release never becomes the repository's latest release.
- The workflow creates the git tag when it publishes. A tag that already exists stops a real run, so never push a `v*` tag by hand.

## One-time setup

A repository admin creates the `release` environment, limits it to `main`, and adds the reviewers who approve a release. A real run waits there after its gates pass. Until the environment has reviewers, a real run does not wait.

## Before a release

- The `conformance` check is green on main, on amd64 and arm64.
- `go.mod` pins fiberd at a release tag, with no `replace`.
- The `FIBERD_VERSION` repository variable is unset or equals `go.mod`.

`make check-pin` reports the last two. A real run fails on them, and a dry run only warns.

## Run a release

Start a dry run, and read it through.

```bash
gh workflow run release.yml --repo Azure/fiberd-k8s-controller --ref main -f version=v0.1.0 -f dry_run=true
```

When it is green, start the release.

```bash
gh workflow run release.yml --repo Azure/fiberd-k8s-controller --ref main -f version=v0.1.0 -f dry_run=false
```

The run tests the commit main is at when it starts, and it releases that commit. Approve the `release` environment once the gates are green. The run then writes a draft, pushes and attests, and publishes the draft in one step. `check-pull` then reads every artifact with no login.

## When a run fails

| Where it stopped | What is left | What to do |
| --- | --- | --- |
| A gate, `build` or `smoke` | Nothing | Re-run the failed jobs, or fix main and dispatch again |
| `draft` | A draft | Re-run the failed jobs |
| `push` | A draft, and perhaps an untagged image, a version tag or a chart | Re-run the failed jobs |
| `publish` | A draft, or a live release | Re-run the failed jobs. A live release with this run's image reports finished |
| `check-pull` | A live release | Set any private package to public, then re-run this job alone |
| Any job, after 7 days | As above | Dispatch the same version again, since the run's artifacts have expired |

Re-running all jobs is safe. It builds new bytes with a new digest and refreshes the draft, and it stops at once on a tag that is already released.

Every job that writes refuses a released tag, and only a complete listing of releases proves a tag is free. `publish` also requires the draft to list this run's digest, so an old run cannot publish a newer run's draft. It then requires the image and chart tags to hold what the release names. An old run that is re-run can still tag its own bytes under the version, and then `publish` stops. Dispatch the version again to finish.

Publish only through the workflow. A draft published by hand skips these checks and leaves a running job free to change it.

The workflow deletes nothing. To abandon a version, delete its draft by hand with `gh release delete <tag>`. A registry tag it left stays until a later run of that version replaces it, and no release points at it.

## What a dry run does not reach

A dry run runs every gate, builds everything, and pulls and runs the image on both architectures through a registry on the runner. These stay for a real run.

- The rules on the ref, the repository and the tag.
- Login to the registry, and a new package's first push.
- The attestations, which are public and permanent.
- The draft, the publish step and the git tag.
- Package visibility and `check-pull`.

A pre-release is the cheap way to reach them.

## Verify a release

Each release's notes give the image and chart digests and these commands.

```bash
sha256sum -c SHA256SUMS
```

```bash
gh attestation verify install.yaml --repo Azure/fiberd-k8s-controller
```

```bash
gh attestation verify oci://ghcr.io/azure/fiberd-k8s-controller@<digest> --repo Azure/fiberd-k8s-controller
```
