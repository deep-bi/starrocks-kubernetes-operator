# Node groups

A `StarRocksNodeGroup` is a named set of identical pods that belongs to one `StarRocksCluster` and is placed, sized
and scaled on its own. A cluster has one spec per role, so all its CN pods are identical and its FE has no observers;
node groups add more sets of CN pods (other instance types, zones, spot capacity) and FE observers.

| Role          | What the pods are                                                              |
|---------------|--------------------------------------------------------------------------------|
| `cn`          | Compute nodes. They join the same pool as the CN of the cluster.               |
| `fe-observer` | FE observers. They serve queries and take no part in the metadata quorum.      |

FE followers and BE stay in the `StarRocksCluster`.

## Install

The CRD is optional and is not part of the Helm chart. Install it before the operator starts, or restart the
operator after installing it: the node group controller only starts when it finds the CRD.

```bash
kubectl apply --server-side -f deploy/starrocks.com_starrocksnodegroups.yaml
```

## Example

```yaml
apiVersion: starrocks.com/v1
kind: StarRocksNodeGroup
metadata:
  name: spot
  namespace: starrocks        # the namespace of the cluster
spec:
  starRocksCluster: sr        # immutable
  role: cn                    # cn | fe-observer, immutable
  template:
    replicas: 2
    requests: {cpu: 4, memory: 16Gi}
    limits: {cpu: 4, memory: 16Gi}
    nodeSelector:
      karpenter.sh/capacity-type: spot
---
apiVersion: starrocks.com/v1
kind: StarRocksNodeGroup
metadata:
  name: readers
  namespace: starrocks
spec:
  starRocksCluster: sr
  role: fe-observer
  template:
    replicas: 2
```

`template` takes the fields of `starRocksCnSpec` or `starRocksFeSpec`, with `envVars` for environment variables.
The group `spot` runs the StatefulSet `spot-cn` with the Services `spot-cn-service` and `spot-cn-search`; the group
`readers` runs `readers-fe`, `readers-fe-service` and `readers-fe-search`. Observers are reached through their own
Service. The FE Service of the cluster does not select them unless the cluster asks for it, see below.

## One FE Service for the cluster and its observers

By default `<cluster>-fe-service` reaches only the FE pods of the cluster. To make it reach the observers of every
node group of the cluster as well, set this annotation on the StarRocksCluster:

```yaml
metadata:
  annotations:
    starrocks.com/fe-service-includes-observers: "true"
```

The operator then adds the label `app.starrocks.io/fe-service: <cluster>` to the FE pods of the cluster. This changes
the FE pod template, so **the FE does one rolling restart**. When every FE pod carries the label, the Service moves
to it and starts to select the observers too. Observers always carry the label and are not restarted. The Services
of the groups stay as they are.

Removing the annotation moves the Service back at once and rolls the FE once more to remove the label.

## What a group takes from its cluster

| What          | Rule                                                                                                   |
|---------------|--------------------------------------------------------------------------------------------------------|
| Image tag     | Always the tag the cluster runs. A group cannot choose a version.                                      |
| Image name    | `template.image`, without a tag (for example `registry.local/starrocks/cn-ubuntu`). Empty: the image of the cluster's component of that role. A CN group of a cluster without `starRocksCnSpec` must set it, and gets the tag of the FE image. |
| Configuration | `template.configMapInfo`, a reference to a ConfigMap. Empty: the ConfigMap of the cluster's component of that role. There is no other way to pass `cn.conf` or `fe.conf`. |
| Root password | `MYSQL_PWD` in `template.envVars`. Not set: the `MYSQL_PWD` of the cluster.                             |

A group with a tag or a digest in `template.image` is not deployed and reports `InvalidSpec`.

## Upgrade

Change the image of the cluster. Every group of the cluster follows: it waits until the FE StatefulSet of the cluster
runs the new image on all its pods, and then rolls its own pods. Groups roll at the same time as the BE and CN of the
cluster and as each other.

## Scale

`spec.template.replicas` is the only place the size of a group is set. The CRD has a `scale` subresource, so
`kubectl scale`, an HPA and a KEDA `ScaledObject` can target the group:

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: spot
spec:
  scaleTargetRef:
    apiVersion: starrocks.com/v1
    kind: StarRocksNodeGroup
    name: spot
  minReplicaCount: 1
  maxReplicaCount: 10
  triggers:
    - type: cpu
      metricType: Utilization
      metadata:
        value: "70"
```

Pods register themselves with the FE when they start. When a group is scaled in, the operator drops from the FE the
nodes whose pod is gone, and no other node.

## Status

```bash
kubectl get starrocksnodegroups     # short names: nodegroups, srng
NAME      CLUSTER   ROLE          DESIRED   READY   REGISTERED   PHASE
readers   sr        fe-observer   2         2       2            running
spot      sr        cn            2         2       2            running
```

| Condition      | True when                                                                                              |
|----------------|--------------------------------------------------------------------------------------------------------|
| `ClusterReady` | The cluster exists, is not in disaster recovery, and its FE is ready. While false the group changes nothing. |
| `Available`    | Every pod the spec asks for is ready.                                                                   |
| `Registered`   | The FE lists every pod the spec asks for as alive, and no pod of the group beyond that.                |
| `Ready`        | The three above are true and every pod runs the current spec. This is the one to alert on.             |

`status.registeredReplicas` counts the nodes the FE lists as alive, and `status.image` is the image resolved from the
cluster.

## Delete

Deleting a group removes its StatefulSet, waits for the pods to be gone and then drops their nodes from the FE. If
the FE does not answer for two minutes, or the cluster is gone, the group is deleted anyway and the nodes stay in the
FE as dead nodes. Set the annotation `starrocks.com/skip-fe-cleanup: "true"` on the group to leave the nodes in the FE
on purpose.

## Limits

- A group lives in the namespace of its cluster.
- A group does not route workloads: without warehouses the FE treats all compute nodes as one pool.
- The name of a group must not produce a StatefulSet name that is taken: the group `sr` of role `cn` would run
  `sr-cn`, the CN StatefulSet of the cluster `sr`. Such a group is not deployed and reports the conflict.
- During a disaster recovery of the cluster, groups wait and deploy nothing until it is done.
