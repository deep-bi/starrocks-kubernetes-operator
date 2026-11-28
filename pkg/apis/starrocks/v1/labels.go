/*
Copyright 2021-present, StarRocks Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

// the labels key
const (
	// ComponentLabelKey is Kubernetes recommended label key, it represents the component within the architecture
	ComponentLabelKey string = "app.kubernetes.io/component"

	// OwnerReference represents  owner of the object
	OwnerReference string = "app.starrocks.ownerreference/name"

	// ComponentResourceHash the component hash
	ComponentResourceHash string = "app.starrocks.components/hash"

	// FEServiceLabelKey is the label the FE service of a cluster selects on once the cluster has opted in with
	// FEServiceIncludesObserversAnnotation. Its value is the name of the cluster. The FE pods of the cluster
	// carry it only after the opt-in; the observers of its node groups always carry it.
	FEServiceLabelKey string = "app.starrocks.io/fe-service"
)

// FEServiceIncludesObserversAnnotation is set to "true" on a StarRocksCluster to make its FE service select
// the FE observers of its node groups as well. Setting or removing it changes the FE pod template, so the FE
// rolls once.
const FEServiceIncludesObserversAnnotation = "starrocks.com/fe-service-includes-observers"

// FEServiceIncludesObservers reports whether the cluster has opted in.
func FEServiceIncludesObservers(cluster *StarRocksCluster) bool {
	return cluster != nil && cluster.Annotations[FEServiceIncludesObserversAnnotation] == "true"
}

// the labels value. default statefulset name
const (
	DEFAULT_FE       = "fe"
	DEFAULT_BE       = "be"
	DEFAULT_CN       = "cn"
	DEFAULT_FE_PROXY = "fe-proxy"
)

// the env of container
const (
	COMPONENT_NAME  = "COMPONENT_NAME"
	FE_SERVICE_NAME = "FE_SERVICE_NAME"
)
