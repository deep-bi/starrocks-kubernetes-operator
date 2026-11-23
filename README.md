# Deep.BI StarRocks Operator

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

## Overview

Deep.BI maintains a patched fork of the
[StarRocks Kubernetes Operator](https://github.com/StarRocks/starrocks-kubernetes-operator) with additional features,
bug fixes and security updates. It deploys and operates StarRocks Frontend (FE), Backend (BE) and Compute Node (CN)
components on Kubernetes.

The fork uses the same CRDs and Helm chart names as the upstream operator, and every addition is either a new optional
field or an operator flag that is off by default, so it is a drop-in replacement for the upstream operator. Changes are
contributed back upstream where applicable.

Releases are published here:
[Deep.BI StarRocks Operator Releases](https://github.com/deep-bi/starrocks-kubernetes-operator/releases)

## Features not in upstream

The table lists what this fork adds on top of upstream `v1.11.8`. "Since" is the first Deep.BI release that has it.

| Feature | How to use it | Since | Upstream PR |
|---|---|---|---|
| **Leader-aware FE upgrade**: followers are replaced first and the leader last, so an upgrade causes one leader election. [Doc](./doc/fe_leader_aware_upgrade_howto.md) | Operator flag `--enable-fe-leader-aware-upgrade`, Helm `starrocksOperator.enableFeLeaderAwareUpgrade` | deepbi.2 | none |
| **Tablet-balance-aware BE rolling update**: one BE is replaced at a time, and the next one waits until tablets are balanced. | `starRocksBeSpec.waitForTabletBalance: true` | deepbi.2 | [#764](https://github.com/StarRocks/starrocks-kubernetes-operator/pull/764) |
| **Persistent volume expansion**: increasing `storageSize` expands the volumes without restarting pods. [Doc](./doc/mount_persistent_volume_howto.md) | Operator flag `--enable-volume-expansion`, Helm `starrocksOperator.enableVolumeExpansion` | deepbi.4 | [#806](https://github.com/StarRocks/starrocks-kubernetes-operator/pull/806) |
| **CN deregistration in shared-nothing clusters**: compute nodes removed by a scale-in are dropped from FE. Upstream does this only in shared-data clusters. | Always on | deepbi.4 | [#802](https://github.com/StarRocks/starrocks-kubernetes-operator/pull/802) |
| **VolumeAttributesClass**: set the IOPS and throughput class of FE, BE and CN volumes. | `storageVolumes[].volumeAttributesClassName` | deepbi.2 | [#762](https://github.com/StarRocks/starrocks-kubernetes-operator/pull/762) |
| **Cross-namespace warehouse**: a `StarRocksWarehouse` can live in a different namespace from its `StarRocksCluster`. | `spec.starRocksClusterNamespace` on the warehouse | deepbi.2 | [#759](https://github.com/StarRocks/starrocks-kubernetes-operator/pull/759) |
| **Priority class**: `priorityClassName` for FE, BE, CN, FE Proxy and warehouse pods, in the CRDs and the Helm charts. | `priorityClassName` on each component spec | deepbi.3 | [#647](https://github.com/StarRocks/starrocks-kubernetes-operator/pull/647) |

Fixes not in upstream:

| Fix | Since | Upstream PR |
|---|---|---|
| `waitForFullRollout` no longer lets BE and CN start upgrading before the FE rollout has been observed. | deepbi.4 | [#801](https://github.com/StarRocks/starrocks-kubernetes-operator/pull/801) |
| Helm: FE Proxy pod annotations are applied. | deepbi.2 | [#763](https://github.com/StarRocks/starrocks-kubernetes-operator/pull/763) |
| Security: operator built with Go 1.25.13 and patched `golang.org/x/net` and `golang.org/x/text`. | deepbi.1 | none |

## Installation

The Helm charts are hosted at `https://charts.deep.bi`.

```bash
helm repo add deep-bi https://charts.deep.bi
helm repo update
```

Install the operator and a StarRocks cluster together:

```bash
helm install starrocks deep-bi/kube-starrocks --version 1.11.8-deepbi.3 -n starrocks --create-namespace
```

Or install only the operator, and manage StarRocks clusters yourself:

```bash
helm install starrocks-operator deep-bi/operator --version 1.11.8-deepbi.3 -n starrocks --create-namespace
```

The charts are `kube-starrocks` (operator and cluster), `operator`, `starrocks` (cluster only) and `warehouse`. Always
pass `--version`: chart versions carry the `-deepbi.N` suffix, which Helm treats as a pre-release and skips otherwise.

To upgrade to a specific version:

```bash
helm upgrade starrocks-operator deep-bi/operator --version 1.11.8-deepbi.3 -n starrocks
```

Helm does not upgrade CRDs. Apply them from the release you are upgrading to:

```bash
kubectl apply --server-side -f https://raw.githubusercontent.com/deep-bi/starrocks-kubernetes-operator/v1.11.8-deepbi.3/deploy/starrocks.com_starrocksclusters.yaml
kubectl apply --server-side -f https://raw.githubusercontent.com/deep-bi/starrocks-kubernetes-operator/v1.11.8-deepbi.3/deploy/starrocks.com_starrockswarehouses.yaml
```

To install without Helm, apply the CRDs above and then the operator manifest:

```bash
kubectl apply -f https://raw.githubusercontent.com/deep-bi/starrocks-kubernetes-operator/v1.11.8-deepbi.3/deploy/operator.yaml
```

## Versioning and Compatibility

Each release is tagged `v<upstream version>-deepbi.<N>` (e.g. `v1.11.8-deepbi.3`): the upstream release it is built on,
followed by the Deep.BI revision. The Helm charts have the same version without the leading `v`.

Docker images are published to Docker Hub as
[`techdeepbi/starrocks`](https://hub.docker.com/r/techdeepbi/starrocks) with the tag `operator-v<version>`
(e.g. `techdeepbi/starrocks:operator-v1.11.8-deepbi.3`).

Requirements: Kubernetes >= 1.23 and Helm >= 3.0.

## Documentation

- [How-to guides](./doc)
- [API reference](./doc/api.md)
- [Example StarRocksCluster manifests](./examples/starrocks)
- [Deploy with the operator manifest](./doc/deploy_starrocks_with_operator_howto.md)
- [Deploy with Helm](./doc/deploy_starrocks_with_helm_howto.md)
- [Upstream documentation on docs.starrocks.io](https://docs.starrocks.io/docs/deployment/sr_operator/)

## Support

For questions or bug reports, open an issue at
[deep-bi/starrocks-kubernetes-operator](https://github.com/deep-bi/starrocks-kubernetes-operator/issues).
