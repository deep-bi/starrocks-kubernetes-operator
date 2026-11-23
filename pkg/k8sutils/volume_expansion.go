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

package k8sutils

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/StarRocks/starrocks-kubernetes-operator/cmd/config"
)

var (
	// recreateTimeout is how long one reconcile waits for a StatefulSet deleted for a volume expansion to be gone.
	// The garbage collector normally releases the pods within a few seconds.
	recreateTimeout = 30 * time.Second
	// recreateInterval is how often that is checked.
	recreateInterval = 200 * time.Millisecond
	// cacheSyncTimeout is how long the cache is given to see the StatefulSet that was created again.
	cacheSyncTimeout = 3 * time.Second
)

// expandVolumeClaims grows the claims of a grown storageSize, then deletes the StatefulSet without its pods and creates
// it again from its live state with the grown volumeClaimTemplates, which can not be changed in place.
// It does nothing unless the operator was started with --enable-volume-expansion.
func expandVolumeClaims(ctx context.Context, k8sClient client.Client, expect, actual *appsv1.StatefulSet) (bool, error) {
	if !config.EnableVolumeExpansion {
		return false, nil
	}
	grown, err := grownClaimTemplates(expect.Spec.VolumeClaimTemplates, actual.Spec.VolumeClaimTemplates)
	if err != nil || len(grown) == 0 {
		return false, err
	}

	// If the operator is interrupted after the delete, the StatefulSet is created from expect instead of from its
	// live state. expect must then keep what the pods depend on.
	if !equality.Semantic.DeepEqual(expect.Spec.Selector, actual.Spec.Selector) ||
		expect.Spec.ServiceName != actual.Spec.ServiceName ||
		expect.Spec.PodManagementPolicy != actual.Spec.PodManagementPolicy {
		return true, fmt.Errorf("can not expand the volumes of statefulset %s: recreating it would change "+
			"its selector, serviceName or podManagementPolicy, which were set by an older operator version", actual.Name)
	}

	// Claims are found by name, the way the StatefulSet controller finds them: one that was created by hand does
	// not carry the labels of the StatefulSet.
	logger := logr.FromContextOrDiscard(ctx)
	var pvcs corev1.PersistentVolumeClaimList
	if err := k8sClient.List(ctx, &pvcs, client.InNamespace(actual.Namespace)); err != nil {
		return true, err
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		size, ok := grown[claimTemplateName(pvc.Name, actual.Name, grown)]
		if !ok || pvc.Spec.Resources.Requests.Storage().Cmp(size) >= 0 {
			continue
		}
		patch := client.MergeFrom(pvc.DeepCopy())
		if pvc.Spec.Resources.Requests == nil {
			pvc.Spec.Resources.Requests = corev1.ResourceList{}
		}
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = size
		logger.Info("expand persistent volume claim", "name", pvc.Name, "size", size.String())
		// the API server refuses this unless the claim's StorageClass sets allowVolumeExpansion
		if err := k8sClient.Patch(ctx, pvc, patch); err != nil {
			return true, fmt.Errorf("expand persistent volume claim %s: %w", pvc.Name, err)
		}
	}

	return true, recreateStatefulSet(ctx, k8sClient, actual, grown)
}

// resumeVolumeExpansion finishes a volume expansion whose StatefulSet is still being deleted, because the garbage
// collector was slow to release the pods or the operator was restarted meanwhile. A StatefulSet that is being deleted
// for any other reason is left alone, and is created from expect once it is gone.
func resumeVolumeExpansion(ctx context.Context, k8sClient client.Client, expect, actual *appsv1.StatefulSet) error {
	if !config.EnableVolumeExpansion {
		return nil
	}
	grown, err := grownClaimTemplates(expect.Spec.VolumeClaimTemplates, actual.Spec.VolumeClaimTemplates)
	if err != nil || len(grown) == 0 {
		return nil
	}
	return recreateStatefulSet(ctx, k8sClient, actual, grown)
}

// recreateStatefulSet deletes a StatefulSet without its pods and creates it again with the grown volumeClaimTemplates.
// Everything else is taken from the live object and not from the StarRocksCluster, so the pods are adopted without
// being restarted even when the StatefulSet carries what the StarRocksCluster does not know, such as the pod template
// annotation that kubectl rollout restart adds. What differs from the StarRocksCluster is applied by the next
// reconcile, like any other update.
func recreateStatefulSet(ctx context.Context, k8sClient client.Client, actual *appsv1.StatefulSet,
	grown map[string]resource.Quantity) error {
	logger := logr.FromContextOrDiscard(ctx)
	key := client.ObjectKeyFromObject(actual)

	// Read without the cache and without this operator's API types, which drop the fields they do not know.
	live := newUnstructuredStatefulSet()
	if err := k8sClient.Get(ctx, key, live); err != nil {
		return client.IgnoreNotFound(err)
	}
	if live.GetUID() != actual.UID {
		return nil // replaced since the cache was read, the next reconcile looks at the new one
	}
	recreated, err := withGrownClaimTemplates(live, grown)
	if err != nil {
		return err
	}

	if live.GetDeletionTimestamp() == nil {
		logger.Info("delete statefulset with orphan propagation to update its volumeClaimTemplates", "name", actual.Name)
		uid, resourceVersion := live.GetUID(), live.GetResourceVersion()
		if err := k8sClient.Delete(ctx, live, client.PropagationPolicy(metav1.DeletePropagationOrphan),
			client.Preconditions{UID: &uid, ResourceVersion: &resourceVersion}); err != nil {
			return err
		}
	} else {
		logger.Info("statefulset is still being deleted to update its volumeClaimTemplates, wait for its pods to be released",
			"name", actual.Name)
	}

	// The name is free once the garbage collector has released the pods.
	replaced := false
	err = wait.PollImmediateWithContext(ctx, recreateInterval, recreateTimeout, func(ctx context.Context) (bool, error) {
		current := newUnstructuredStatefulSet()
		if err := k8sClient.Get(ctx, key, current); err != nil {
			return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
		}
		replaced = current.GetUID() != live.GetUID()
		return replaced, nil
	})
	if err != nil {
		return fmt.Errorf("statefulset %s is still being deleted, it is created again once its pods are released: %w",
			actual.Name, err)
	}
	if replaced {
		return nil
	}

	logger.Info("create statefulset again with the expanded volumeClaimTemplates", "name", actual.Name)
	if err := k8sClient.Create(ctx, recreated); err != nil {
		return client.IgnoreAlreadyExists(err)
	}

	// Give the cache a moment to see the new StatefulSet, so that the rest of this reconcile does not find it missing.
	_ = wait.PollImmediateWithContext(ctx, 20*time.Millisecond, cacheSyncTimeout, func(ctx context.Context) (bool, error) {
		var cached appsv1.StatefulSet
		err := k8sClient.Get(ctx, key, &cached)
		return err == nil && cached.UID == recreated.GetUID(), nil
	})
	return nil
}

func newUnstructuredStatefulSet() *unstructured.Unstructured {
	sts := &unstructured.Unstructured{}
	sts.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("StatefulSet"))
	return sts
}

// withGrownClaimTemplates returns a StatefulSet to create in place of live: the same object, with the new size in its
// grown volumeClaimTemplates and without what belongs to the object that is being deleted.
func withGrownClaimTemplates(live *unstructured.Unstructured, grown map[string]resource.Quantity) (
	*unstructured.Unstructured, error) {
	spec, _, err := unstructured.NestedMap(live.Object, "spec")
	if err != nil {
		return nil, err
	}
	templates, _, err := unstructured.NestedSlice(spec, "volumeClaimTemplates")
	if err != nil {
		return nil, err
	}
	for i := range templates {
		template, ok := templates[i].(map[string]interface{})
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(template, "metadata", "name")
		size, ok := grown[name]
		if !ok {
			continue
		}
		if err := unstructured.SetNestedField(template, size.String(), "spec", "resources", "requests", "storage"); err != nil {
			return nil, err
		}
	}
	if len(templates) > 0 {
		if err := unstructured.SetNestedSlice(spec, templates, "volumeClaimTemplates"); err != nil {
			return nil, err
		}
	}

	recreated := newUnstructuredStatefulSet()
	recreated.SetNamespace(live.GetNamespace())
	recreated.SetName(live.GetName())
	recreated.SetLabels(live.GetLabels())
	recreated.SetAnnotations(live.GetAnnotations())
	recreated.SetOwnerReferences(live.GetOwnerReferences())
	if err := unstructured.SetNestedMap(recreated.Object, spec, "spec"); err != nil {
		return nil, err
	}
	return recreated, nil
}

// grownClaimTemplates returns the new size of each grown template; nothing if the templates differ in anything else.
func grownClaimTemplates(expect, actual []corev1.PersistentVolumeClaim) (map[string]resource.Quantity, error) {
	if len(expect) != len(actual) {
		return nil, nil
	}
	grown := map[string]resource.Quantity{}
	for i := range expect {
		e, a := &expect[i], &actual[i]
		if e.Name != a.Name || !equality.Semantic.DeepEqual(e.Spec.StorageClassName, a.Spec.StorageClassName) {
			return nil, nil
		}
		want, have := e.Spec.Resources.Requests.Storage(), a.Spec.Resources.Requests.Storage()
		switch want.Cmp(*have) {
		case 1:
			grown[e.Name] = want.DeepCopy()
		case -1:
			return nil, fmt.Errorf("storageSize of volume %s can not be decreased from %s to %s",
				e.Name, have.String(), want.String())
		}
	}
	return grown, nil
}

// claimTemplateName returns the template a claim was created from, named <template>-<statefulset>-<ordinal>.
func claimTemplateName(claimName, stsName string, templates map[string]resource.Quantity) string {
	for name := range templates {
		ordinal, found := strings.CutPrefix(claimName, name+"-"+stsName+"-")
		if _, err := strconv.Atoi(ordinal); found && err == nil {
			return name
		}
	}
	return ""
}
