# The cloudquarter fork

This repository is cloudquarter's fork of
[ionos-cloud/cluster-api-provider-proxmox](https://github.com/ionos-cloud/cluster-api-provider-proxmox).
It exists to run one Cluster API cluster across availability zones that are
separate Proxmox clusters, each with its own subnet. Every change is kept
additive and contributable upstream.

## Branches

- `cloudquarter` is the integration branch and the default. It is upstream
  `main` plus the changes below; upstream is merged into it when upstream
  releases (see "Syncing with upstream").
- `main` mirrors upstream and is never committed to.
- Work lands through pull requests against `cloudquarter`.

## What the fork adds

- Upstream's open failure-domain contract for zones (ionos-cloud PR #720):
  `zoneConfig` entries become CAPI failure domains, each with its own nodes.
- Per-zone endpoint and clone inputs on `ZoneConfigSpec`: `credentialsRef`,
  `templateSource`, `storage` and `networkDevices`. See
  `docs/advanced-setups.md`, "One Proxmox cluster per zone".
- A credentials Secret in another namespace than its ProxmoxCluster is
  recorded as used with a label, not an owner reference, so Kubernetes does
  not delete it. See `docs/advanced-setups.md`, "Credentials Secrets".
- A zone's `dnsServers` reach the guest network configuration of the
  machines placed in it.
- `status.proxmoxNode` follows a VM Proxmox HA moved
  (`--machine-location-resync`). See `docs/advanced-setups.md`, "VMs moved by
  Proxmox HA".
- A provisioning failure a new VM can fix clones one under the same
  ProxmoxMachine, up to `--provision-retry-limit`. See
  `docs/advanced-setups.md`, "Retrying a failed provisioning".
- A running VM is stopped before it is deleted.

## Syncing with upstream

When upstream releases, merge the release into `cloudquarter` through a pull
request:

1. Fast-forward the fork's `main` to upstream `main`
   (`git push origin upstream/main:main`).
2. On a branch from `cloudquarter`, merge upstream's release tag, for example
   `git merge v0.9.2`, not `upstream/main`, which can carry unreleased
   commits. Resolve the conflicts and open the pull request against
   `cloudquarter`.
3. Land it with a merge commit, not a squash. A squash hides upstream's
   commits from Git, and the next sync conflicts on the same lines again.

Where upstream and the fork fixed the same thing, keep upstream's names and
the fork's behaviour unless the pull request says otherwise. A bootstrap ISO
Proxmox VE fails to take (upstream's `inject.ISOError`) clones a new VM under
the reprovisioning limit; upstream retries the same VM.

Machine code reaches Proxmox VE through `MachineScope.ProxmoxClient()`, which
resolves the machine's zone. Upstream code reads the ProxmoxCluster's default
client, for example `machineScope.InfraCluster.ProxmoxClient`; it compiles
after a merge and reaches the wrong Proxmox cluster, or none when every zone
carries its own `credentialsRef`. `TestMachineCodeReachesProxmoxThroughItsZone`
in `pkg/scope` fails on any read of a default-client field outside
`pkg/scope`, however it is spelled, other than handing it to
`ClusterScopeParams`.

Do not push upstream's tags to the fork. A `v*` tag runs the workflows of
the tagged commit, upstream's: `container-image` would push an image built
from upstream's code to the fork's registry under that tag, and `release`
would leave a draft release whose manifest names upstream's image. Never run
`git push --tags` from a clone that fetched upstream. The upstream tags up to
`v0.9.1` came with the fork when it was created.

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
`GOTOOLCHAIN=go1.26.10 bin/golangci-lint-kube-api-linter run --config .golangci-kal.yml ./...`.
The e2e workflows need a Proxmox lab the fork does not have; pull requests
carry the `e2e/none` label.
