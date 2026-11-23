// Copyright 2021-present, StarRocks Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package be

import (
	"context"
	"errors"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	srapi "github.com/StarRocks/starrocks-kubernetes-operator/pkg/apis/starrocks/v1"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/k8sutils/fake"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/k8sutils/load"
	subc "github.com/StarRocks/starrocks-kubernetes-operator/pkg/subcontrollers"
)

// The rollout gate returns a RequeueAfterError to hold the whole reconcile. It may only do so while a BE pod still
// runs an older revision: a pod that is merely missing or not ready must not keep CN from being synced.
func TestManageOnDeleteRollout(t *testing.T) {
	const (
		stsName     = "sr-be"
		oldRevision = "sr-be-old"
		newRevision = "sr-be-new"
	)
	src := &srapi.StarRocksCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "sr", Namespace: "default"},
		Spec: srapi.StarRocksClusterSpec{
			StarRocksBeSpec: &srapi.StarRocksBeSpec{WaitForTabletBalance: true},
		},
	}
	pod := func(ordinal int, revision string) *corev1.Pod {
		labels := map[string]string{appsv1.ControllerRevisionHashLabelKey: revision}
		for k, v := range load.Selector(src.Name, src.Spec.StarRocksBeSpec) {
			labels[k] = v
		}
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-%d", stsName, ordinal), Namespace: src.Namespace, Labels: labels,
		}}
	}
	// a StatefulSet of 3 that the OnDelete strategy has rolled before: CurrentRevision stays behind for good
	sts := func(ready, updated int32, mutate func(*appsv1.StatefulSet)) *appsv1.StatefulSet {
		replicas := int32(3)
		s := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: src.Namespace, Generation: 2},
			Spec:       appsv1.StatefulSetSpec{Replicas: &replicas},
			Status: appsv1.StatefulSetStatus{
				ObservedGeneration: 2,
				CurrentRevision:    oldRevision,
				UpdateRevision:     newRevision,
				ReadyReplicas:      ready,
				UpdatedReplicas:    updated,
			},
		}
		if mutate != nil {
			mutate(s)
		}
		return s
	}

	tests := []struct {
		name        string
		objects     []runtime.Object
		wantRequeue bool
	}{
		{
			name:    "every pod runs the update revision",
			objects: []runtime.Object{sts(3, 3, nil), pod(0, newRevision), pod(1, newRevision), pod(2, newRevision)},
		},
		{
			name: "statefulset not in the cache yet",
		},
		{
			name: "status does not describe the latest spec",
			objects: []runtime.Object{
				sts(2, 0, func(s *appsv1.StatefulSet) { s.Generation = 3 }),
				pod(0, oldRevision), pod(1, oldRevision), pod(2, oldRevision),
			},
		},
		{
			name: "statefulset is being deleted",
			objects: []runtime.Object{
				sts(2, 0, func(s *appsv1.StatefulSet) {
					now := metav1.Now()
					s.DeletionTimestamp = &now
					s.Finalizers = []string{metav1.FinalizerOrphanDependents}
				}),
				pod(0, oldRevision), pod(1, oldRevision), pod(2, oldRevision),
			},
		},
		{
			name:    "a pod is missing and no pod is outdated",
			objects: []runtime.Object{sts(2, 2, nil), pod(0, newRevision), pod(1, newRevision)},
		},
		{
			name:    "a pod is being created and no pod is outdated",
			objects: []runtime.Object{sts(2, 3, nil), pod(0, newRevision), pod(1, newRevision), pod(2, newRevision)},
		},
		{
			name:        "a pod is outdated while another is not ready",
			objects:     []runtime.Object{sts(2, 2, nil), pod(0, oldRevision), pod(1, newRevision), pod(2, newRevision)},
			wantRequeue: true,
		},
		{
			// created again for a volume expansion: both revisions are equal, the adopted pods are behind
			name: "pods are outdated although the revisions are equal",
			objects: []runtime.Object{
				sts(2, 0, func(s *appsv1.StatefulSet) { s.Status.CurrentRevision = newRevision }),
				pod(0, oldRevision), pod(1, oldRevision), pod(2, oldRevision),
			},
			wantRequeue: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := &BeController{Client: fake.NewFakeClient(srapi.Scheme, tt.objects...)}
			err := controller.manageOnDeleteRollout(context.Background(), src, stsName)
			var requeue *subc.RequeueAfterError
			if got := errors.As(err, &requeue); got != tt.wantRequeue {
				t.Fatalf("held the reconcile = %v (error: %v), want %v", got, err, tt.wantRequeue)
			}
			if !tt.wantRequeue && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
