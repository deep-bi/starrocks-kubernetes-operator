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

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeGroupRole is the StarRocks role every pod of a node group runs.
// +kubebuilder:validation:Enum=cn;fe-observer
type NodeGroupRole string

const (
	// NodeGroupRoleCN runs compute nodes.
	NodeGroupRoleCN NodeGroupRole = "cn"
	// NodeGroupRoleFEObserver runs FE observers: frontends that serve queries and take no part in the
	// metadata quorum.
	NodeGroupRoleFEObserver NodeGroupRole = "fe-observer"
)

const (
	// NodeGroupFinalizer keeps a node group until its nodes have been dropped from the FE.
	NodeGroupFinalizer = "starrocks.com/nodegroup-protection"

	// EnvIsFEObserver makes the FE entrypoint join the cluster as an observer instead of a follower.
	EnvIsFEObserver = "IS_FE_OBSERVER"
)

// Condition types of a StarRocksNodeGroup.
const (
	// NodeGroupClusterReady is true when the cluster the group belongs to exists, is not being restored and
	// has its FE ready. While it is false the group changes nothing.
	NodeGroupClusterReady = "ClusterReady"
	// NodeGroupAvailable is true when every pod the spec asks for is ready.
	NodeGroupAvailable = "Available"
	// NodeGroupRegistered is true when the FE lists every pod the spec asks for as alive, and no other pod
	// of the group.
	NodeGroupRegistered = "Registered"
	// NodeGroupReady is true when the three conditions above are true and every pod runs the current spec.
	NodeGroupReady = "Ready"
)

// StarRocksNodeGroupSpec defines the desired state of StarRocksNodeGroup.
type StarRocksNodeGroupSpec struct {
	// StarRocksCluster is the name of the StarRocksCluster the node group belongs to. The cluster must be in
	// the namespace of the node group.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="starRocksCluster is immutable"
	StarRocksCluster string `json:"starRocksCluster"`

	// Role is the role the pods of the node group run: cn or fe-observer.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="role is immutable"
	Role NodeGroupRole `json:"role"`

	// Template describes the pods of the node group.
	Template NodeGroupTemplate `json:"template"`
}

// NodeGroupTemplate describes the pods of a node group. It takes the fields of a component of
// StarRocksCluster, with two differences:
//   - image is optional and names a repository only, e.g. starrocks/cn-ubuntu. The tag is always the one the
//     cluster runs. When image is empty the group runs the image of the cluster's component of its role.
//   - configMapInfo is optional. When it is empty the group mounts the ConfigMap of the cluster's component
//     of its role.
type NodeGroupTemplate struct {
	StarRocksComponentSpec `json:",inline"`

	// +optional
	// envVars is a slice of environment variables that are added to the pods, the default is empty.
	EnvVars []corev1.EnvVar `json:"envVars,omitempty"`
}

// ToCnSpec returns the template as the spec of a CN component that runs image.
func (template *NodeGroupTemplate) ToCnSpec(image string) *StarRocksCnSpec {
	spec := &StarRocksCnSpec{
		StarRocksComponentSpec: *template.StarRocksComponentSpec.DeepCopy(),
		CnEnvVars:              append([]corev1.EnvVar(nil), template.EnvVars...),
	}
	spec.Image = image
	return spec
}

// ToFeSpec returns the template as the spec of an FE component that runs image.
func (template *NodeGroupTemplate) ToFeSpec(image string) *StarRocksFeSpec {
	spec := &StarRocksFeSpec{
		StarRocksComponentSpec: *template.StarRocksComponentSpec.DeepCopy(),
		FeEnvVars:              append([]corev1.EnvVar(nil), template.EnvVars...),
	}
	spec.Image = image
	return spec
}

// StarRocksNodeGroupStatus defines the observed state of StarRocksNodeGroup.
type StarRocksNodeGroupStatus struct {
	// ObservedGeneration is the generation of the spec this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase summarizes the conditions in one word: reconciling, running or failed.
	// +optional
	Phase ComponentPhase `json:"phase,omitempty"`

	// Image is the image the pods run, resolved from the cluster.
	// +optional
	Image string `json:"image,omitempty"`

	// Replicas is the number of pods of the group that exist.
	// +optional
	Replicas int32 `json:"replicas"`

	// ReadyReplicas is the number of pods that pass readiness.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas"`

	// UpdatedReplicas is the number of pods that run the current spec.
	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas"`

	// RegisteredReplicas is the number of pods of the group the FE lists as alive.
	// +optional
	RegisteredReplicas int32 `json:"registeredReplicas"`

	// Selector selects the pods of the group. It is read by the scale subresource.
	// +optional
	Selector string `json:"selector,omitempty"`

	// Conditions describe the state of the group: ClusterReady, Available, Registered and Ready.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// StarRocksNodeGroup is a named set of identical CN or FE observer pods that belongs to one StarRocksCluster
// and is placed, sized and scaled on its own.
// +kubebuilder:object:root=true
// +kubebuilder:metadata:annotations="version=v1.11.8"
// +kubebuilder:resource:shortName=nodegroups;srng
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.template.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:printcolumn:name="cluster",type=string,JSONPath=`.spec.starRocksCluster`
// +kubebuilder:printcolumn:name="role",type=string,JSONPath=`.spec.role`
// +kubebuilder:printcolumn:name="desired",type=integer,JSONPath=`.spec.template.replicas`
// +kubebuilder:printcolumn:name="ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="registered",type=integer,JSONPath=`.status.registeredReplicas`
// +kubebuilder:printcolumn:name="phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:storageversion
// +k8s:openapi-gen=true
// +genclient
type StarRocksNodeGroup struct {
	metav1.TypeMeta `json:",inline"`
	// +k8s:openapi-gen=false
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec represents the specification of desired state of a node group.
	Spec StarRocksNodeGroupSpec `json:"spec,omitempty"`

	// Status represents the recent observed status of the node group.
	Status StarRocksNodeGroupStatus `json:"status,omitempty"`
}

// StarRocksNodeGroupList contains a list of StarRocksNodeGroup
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:openapi-gen=true
type StarRocksNodeGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StarRocksNodeGroup `json:"items"`
}
