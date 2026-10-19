# The cloudquarter fork

This repository is cloudquarter's fork of
[ionos-cloud/cluster-api-provider-proxmox](https://github.com/ionos-cloud/cluster-api-provider-proxmox).
It exists to run one Cluster API cluster across availability zones that are
separate Proxmox clusters, each with its own subnet. Every change is kept
additive and contributable upstream.

## Branches

- `cloudquarter` is the integration branch and the default. It is upstream
  `main` plus the changes below, and is rebased onto upstream when upstream
  releases.
- `main` mirrors upstream and is never committed to.
- Work lands through pull requests against `cloudquarter`.

## What the fork adds

- Upstream's open failure-domain contract for zones (ionos-cloud PR #720):
  `zoneConfig` entries become CAPI failure domains, each with its own nodes.
- Per-zone endpoint and clone inputs on `ZoneConfigSpec`: `credentialsRef`,
  `templateSource`, `storage` and `networkDevices`. See
  `docs/advanced-setups.md`, "One Proxmox cluster per zone".

## Releases

A release is a `v*` tag on `cloudquarter`. Pushing the tag runs two workflows:

- `container-image` publishes the controller image as
  `ghcr.io/cloudquarter-io/cluster-api-provider-proxmox:<tag>`.
- `release` publishes a GitHub release with `infrastructure-components.yaml`
  (its manager image set to that tag), `metadata.yaml`, the cluster
  templates and a `SHA256SUMS` file. A tag with a pre-release suffix is
  published as a pre-release.

Before tagging, run the `release` workflow by hand ("Run workflow" with a
version): it builds the same assets, checks the manifest references the
published image name, and keeps them as a workflow artifact without creating
a release or pushing an image.

The fork's versions take the next upstream minor with a `-cq.N` suffix, for
example `v0.10.0-cq.1` while upstream's latest release is `v0.9.1`: it sorts
after every upstream release the fork contains and before the upstream
release that will follow. `metadata.yaml` carries a release series for that
minor.

Consumers that fetch the assets without credentials need the package
`cluster-api-provider-proxmox` in the organisation's container registry to be
public; check its visibility after the first publication.

## Checks

`make lint test` as upstream. The kube-api-linter binary in `bin/` reads the
Go toolchain `go.mod` declares; on a newer local Go run it as
`GOTOOLCHAIN=go1.26.6 bin/golangci-lint-kube-api-linter run --config .golangci-kal.yml ./...`.
The e2e workflows need a Proxmox lab the fork does not have; pull requests
carry the `e2e/none` label.
