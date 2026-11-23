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

package k8sutils_test

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/StarRocks/starrocks-kubernetes-operator/cmd/config"
	srapi "github.com/StarRocks/starrocks-kubernetes-operator/pkg/apis/starrocks/v1"
	rutils "github.com/StarRocks/starrocks-kubernetes-operator/pkg/common/resource_utils"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/k8sutils"
)

var selector = map[string]string{"app": "cn"}

func claimTemplate(name, size string) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
}

func claim(name, size string) *corev1.PersistentVolumeClaim {
	c := claimTemplate(name, size)
	c.Namespace, c.Labels = "default", selector
	return &c
}

func sts(serviceName string, templates ...corev1.PersistentVolumeClaim) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-cn", Namespace: "default", Annotations: map[string]string{}},
		Spec: appsv1.StatefulSetSpec{
			Selector:             &metav1.LabelSelector{MatchLabels: selector},
			ServiceName:          serviceName,
			VolumeClaimTemplates: templates,
		},
	}
}

func storageOf(t *testing.T, c client.Client, name string) string {
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &pvc); err != nil {
		t.Fatal(err)
	}
	return pvc.Spec.Resources.Requests.Storage().String()
}

func getSts(t *testing.T, c client.Client) *appsv1.StatefulSet {
	var got appsv1.StatefulSet
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "kube-cn"}, &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func claimTemplateSize(sts *appsv1.StatefulSet) string {
	return sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests.Storage().String()
}

func stsExists(t *testing.T, c client.Client) bool {
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "kube-cn"}, &appsv1.StatefulSet{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

// enableVolumeExpansion turns the --enable-volume-expansion flag on for one test.
func enableVolumeExpansion(t *testing.T) {
	config.EnableVolumeExpansion = true
	t.Cleanup(func() { config.EnableVolumeExpansion = false })
}

func TestApplyStatefulSetLeavesVolumeClaimsWhenExpansionIsDisabled(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(srapi.Scheme).WithObjects(
		sts("kube-cn-search", claimTemplate("cn-data", "10Gi")), claim("cn-data-kube-cn-0", "10Gi"),
	).Build()

	expect := sts("kube-cn-search", claimTemplate("cn-data", "20Gi"))
	if err := k8sutils.ApplyStatefulSet(context.Background(), c, expect, true, rutils.StatefulSetDeepEqual); err != nil {
		t.Fatal(err)
	}
	if got := storageOf(t, c, "cn-data-kube-cn-0"); got != "10Gi" {
		t.Errorf("claim was expanded to %s with volume expansion disabled", got)
	}
	if !stsExists(t, c) {
		t.Fatal("statefulset was deleted with volume expansion disabled")
	}
}

func TestApplyStatefulSetExpandsVolumeClaims(t *testing.T) {
	enableVolumeExpansion(t)
	ctx := context.Background()
	unlabelled := claim("cn-data-kube-cn-2", "10Gi")
	unlabelled.Labels = nil // created by hand, before the StatefulSet
	c := fake.NewClientBuilder().WithScheme(srapi.Scheme).WithObjects(
		sts("kube-cn-search", claimTemplate("cn-data", "10Gi"), claimTemplate("cn-log", "1Gi")),
		claim("cn-data-kube-cn-0", "10Gi"), claim("cn-data-kube-cn-1", "10Gi"), unlabelled, claim("cn-log-kube-cn-0", "1Gi"),
		claim("cn-data-kube-cn-other-0", "10Gi"), // another StatefulSet's claim with the same labels
	).Build()

	expect := sts("kube-cn-search", claimTemplate("cn-data", "20Gi"), claimTemplate("cn-log", "1Gi"))
	if err := k8sutils.ApplyStatefulSet(ctx, c, expect, true, rutils.StatefulSetDeepEqual); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"cn-data-kube-cn-0": "20Gi", "cn-data-kube-cn-1": "20Gi", "cn-data-kube-cn-2": "20Gi",
		"cn-log-kube-cn-0": "1Gi", "cn-data-kube-cn-other-0": "10Gi",
	} {
		if got := storageOf(t, c, name); got != want {
			t.Errorf("%s: got %s, want %s", name, got, want)
		}
	}
	// the statefulset is created again with the new size in the same reconcile
	if got := getSts(t, c); claimTemplateSize(got) != "20Gi" || got.DeletionTimestamp != nil {
		t.Fatalf("statefulset has %s and deletionTimestamp %v, want 20Gi and none", claimTemplateSize(got), got.DeletionTimestamp)
	}

	// the next reconcile has nothing left to expand
	expect = sts("kube-cn-search", claimTemplate("cn-data", "20Gi"), claimTemplate("cn-log", "1Gi"))
	if err := k8sutils.ApplyStatefulSet(ctx, c, expect, true, rutils.StatefulSetDeepEqual); err != nil {
		t.Fatal(err)
	}
	if got := getSts(t, c); claimTemplateSize(got) != "20Gi" {
		t.Fatalf("statefulset has %s, want 20Gi", claimTemplateSize(got))
	}
}

// The StatefulSet is created again from its live state. What the expected StatefulSet does not say, such as a replica
// count it leaves empty or a pod template annotation added by kubectl, must survive, or pods are stopped or restarted.
func TestApplyStatefulSetExpansionKeepsLiveState(t *testing.T) {
	enableVolumeExpansion(t)
	ctx := context.Background()
	const restartedAt = "kubectl.kubernetes.io/restartedAt"

	live := sts("kube-cn-search", claimTemplate("cn-data", "10Gi"))
	replicas := int32(3)
	live.Spec.Replicas = &replicas
	live.Spec.Template.Annotations = map[string]string{restartedAt: "2026-11-23T10:00:00Z"}
	live.Spec.Template.Spec.Containers = []corev1.Container{{Name: "cn", Image: "cn:1"}}
	c := fake.NewClientBuilder().WithScheme(srapi.Scheme).WithObjects(live, claim("cn-data-kube-cn-0", "10Gi")).Build()

	// the expected statefulset leaves replicas empty, and changes the image along with the size
	expect := func() *appsv1.StatefulSet {
		e := sts("kube-cn-search", claimTemplate("cn-data", "20Gi"))
		e.Spec.Template.Spec.Containers = []corev1.Container{{Name: "cn", Image: "cn:2"}}
		return e
	}

	if err := k8sutils.ApplyStatefulSet(ctx, c, expect(), true, rutils.StatefulSetDeepEqual); err != nil {
		t.Fatal(err)
	}
	got := getSts(t, c)
	if claimTemplateSize(got) != "20Gi" {
		t.Errorf("volumeClaimTemplate has %s, want 20Gi", claimTemplateSize(got))
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 3 {
		t.Errorf("replicas = %v, want the 3 of the live statefulset", got.Spec.Replicas)
	}
	if got.Spec.Template.Annotations[restartedAt] == "" {
		t.Errorf("pod template annotations = %v, want %s kept", got.Spec.Template.Annotations, restartedAt)
	}
	if image := got.Spec.Template.Spec.Containers[0].Image; image != "cn:1" {
		t.Errorf("image = %s, want the pod template left as it was", image)
	}

	// the image change is applied by the next reconcile, as an ordinary update
	if err := k8sutils.ApplyStatefulSet(ctx, c, expect(), true, rutils.StatefulSetDeepEqual); err != nil {
		t.Fatal(err)
	}
	got = getSts(t, c)
	if image := got.Spec.Template.Spec.Containers[0].Image; image != "cn:2" {
		t.Errorf("image = %s, want cn:2", image)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 3 || got.Spec.Template.Annotations[restartedAt] == "" {
		t.Errorf("replicas = %v, pod template annotations = %v, want both kept", got.Spec.Replicas, got.Spec.Template.Annotations)
	}
}

// A StatefulSet that is still being deleted when the reconcile gives up waiting, or when the operator restarts, is
// picked up again by a later reconcile.
func TestApplyStatefulSetResumesVolumeExpansion(t *testing.T) {
	enableVolumeExpansion(t)
	ctx := context.Background()
	deleting := func(size string) *appsv1.StatefulSet {
		s := sts("kube-cn-search", claimTemplate("cn-data", size))
		replicas := int32(3)
		s.Spec.Replicas = &replicas
		now := metav1.Now()
		s.DeletionTimestamp = &now
		s.Finalizers = []string{metav1.FinalizerOrphanDependents}
		return s
	}
	expect := func() *appsv1.StatefulSet { return sts("kube-cn-search", claimTemplate("cn-data", "20Gi")) }

	t.Run("pods not released yet", func(t *testing.T) {
		defer k8sutils.SetRecreateTimeout(50 * time.Millisecond)()
		c := fake.NewClientBuilder().WithScheme(srapi.Scheme).WithObjects(deleting("10Gi")).Build()
		if err := k8sutils.ApplyStatefulSet(ctx, c, expect(), true, rutils.StatefulSetDeepEqual); err == nil {
			t.Fatal("expected an error, so that the reconcile is retried")
		}
		if got := getSts(t, c); got.DeletionTimestamp == nil || claimTemplateSize(got) != "10Gi" {
			t.Fatalf("statefulset was touched: deletionTimestamp %v, size %s", got.DeletionTimestamp, claimTemplateSize(got))
		}
	})

	t.Run("pods released", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(srapi.Scheme).WithObjects(deleting("10Gi")).Build()
		released := make(chan error, 1)
		go func() { // the garbage collector releases the pods, then removes its finalizer
			time.Sleep(300 * time.Millisecond)
			var s appsv1.StatefulSet
			if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "kube-cn"}, &s); err != nil {
				released <- err
				return
			}
			s.Finalizers = nil
			released <- c.Update(ctx, &s)
		}()
		if err := k8sutils.ApplyStatefulSet(ctx, c, expect(), true, rutils.StatefulSetDeepEqual); err != nil {
			t.Fatal(err)
		}
		if err := <-released; err != nil {
			t.Fatal(err)
		}
		got := getSts(t, c)
		if got.DeletionTimestamp != nil || claimTemplateSize(got) != "20Gi" {
			t.Errorf("deletionTimestamp %v, size %s, want a new statefulset with 20Gi", got.DeletionTimestamp, claimTemplateSize(got))
		}
		if got.Spec.Replicas == nil || *got.Spec.Replicas != 3 {
			t.Errorf("replicas = %v, want the 3 of the deleted statefulset", got.Spec.Replicas)
		}
	})

	t.Run("deleted for another reason", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(srapi.Scheme).WithObjects(deleting("20Gi")).Build()
		if err := k8sutils.ApplyStatefulSet(ctx, c, expect(), true, rutils.StatefulSetDeepEqual); err != nil {
			t.Fatal(err)
		}
		if got := getSts(t, c); got.DeletionTimestamp == nil {
			t.Fatal("statefulset must be left to be deleted")
		}
	})
}

func TestApplyStatefulSetRefusesVolumeExpansion(t *testing.T) {
	enableVolumeExpansion(t)
	tests := []struct {
		name   string
		expect *appsv1.StatefulSet
	}{
		{name: "decrease", expect: sts("kube-cn-search", claimTemplate("cn-data", "5Gi"))},
		{name: "legacy service name", expect: sts("kube-cn-search-new", claimTemplate("cn-data", "20Gi"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(srapi.Scheme).WithObjects(
				sts("kube-cn-search", claimTemplate("cn-data", "10Gi")), claim("cn-data-kube-cn-0", "10Gi"),
			).Build()
			if err := k8sutils.ApplyStatefulSet(context.Background(), c, tt.expect, true, rutils.StatefulSetDeepEqual); err == nil {
				t.Fatal("expected an error")
			}
			if got := storageOf(t, c, "cn-data-kube-cn-0"); got != "10Gi" {
				t.Errorf("claim changed to %s", got)
			}
			if !stsExists(t, c) {
				t.Error("statefulset must not be deleted")
			}
		})
	}
}

func TestApplyStatefulSetLeavesOtherTemplateChanges(t *testing.T) {
	enableVolumeExpansion(t)
	for name, expect := range map[string]*appsv1.StatefulSet{
		"unchanged":     sts("kube-cn-search", claimTemplate("cn-data", "10Gi")),
		"volume added":  sts("kube-cn-search", claimTemplate("cn-data", "20Gi"), claimTemplate("cn-spill", "10Gi")),
		"volume rename": sts("kube-cn-search", claimTemplate("cn-cache", "20Gi")),
	} {
		c := fake.NewClientBuilder().WithScheme(srapi.Scheme).WithObjects(
			sts("kube-cn-search", claimTemplate("cn-data", "10Gi")), claim("cn-data-kube-cn-0", "10Gi"),
		).Build()
		if err := k8sutils.ApplyStatefulSet(context.Background(), c, expect, true, rutils.StatefulSetDeepEqual); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := storageOf(t, c, "cn-data-kube-cn-0"); got != "10Gi" || !stsExists(t, c) {
			t.Errorf("%s: claim %s, statefulset kept %v", name, got, stsExists(t, c))
		}
	}
}

func TestApplyStatefulSetKeepsOrphanFinalizer(t *testing.T) {
	deleting := sts("kube-cn-search")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{metav1.FinalizerOrphanDependents, "starrocks.com.starrockswarehouse/protection"}
	c := fake.NewClientBuilder().WithScheme(srapi.Scheme).WithObjects(deleting).Build()

	for i := 0; i < 2; i++ {
		if err := k8sutils.ApplyStatefulSet(context.Background(), c, sts("kube-cn-search"), true, rutils.StatefulSetDeepEqual); err != nil {
			t.Fatal(err)
		}
	}
	var got appsv1.StatefulSet
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "kube-cn"}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) != 1 || got.Finalizers[0] != metav1.FinalizerOrphanDependents {
		t.Fatalf("finalizers = %v, want only %s", got.Finalizers, metav1.FinalizerOrphanDependents)
	}
}
