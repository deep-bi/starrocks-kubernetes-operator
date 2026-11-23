package be

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	_ "github.com/go-sql-driver/mysql" // import mysql driver
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	srapi "github.com/StarRocks/starrocks-kubernetes-operator/pkg/apis/starrocks/v1"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/k8sutils"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/k8sutils/load"
	subc "github.com/StarRocks/starrocks-kubernetes-operator/pkg/subcontrollers"
)

const tabletBalanceCheckInterval = 30 * time.Second

// manageOnDeleteRollout implements a controlled rolling update for BE pods.
// With OnDelete strategy, the StatefulSet controller does not automatically
// restart pods when the spec changes. This method deletes one pod at a time
// (highest ordinal first) only after verifying that all tablets are balanced.
func (be *BeController) manageOnDeleteRollout(
	ctx context.Context, src *srapi.StarRocksCluster, stsName string,
) error {
	logger := logr.FromContextOrDiscard(ctx)

	var actual appsv1.StatefulSet
	if err := be.Client.Get(ctx, types.NamespacedName{
		Namespace: src.Namespace,
		Name:      stsName,
	}, &actual); err != nil {
		if apierrors.IsNotFound(err) {
			// Right after the StatefulSet is created the cached client may not see it yet; the watch on
			// StatefulSets triggers another reconcile once it does.
			return nil
		}
		return fmt.Errorf("get BE statefulset: %w", err)
	}

	// Nothing can be told from a StatefulSet that is being deleted, or whose status does not describe its
	// latest spec yet. The next status update of the StatefulSet triggers another reconcile.
	if actual.DeletionTimestamp != nil || actual.Status.ObservedGeneration < actual.Generation ||
		actual.Status.UpdateRevision == "" {
		return nil
	}

	replicas := int32(1)
	if actual.Spec.Replicas != nil {
		replicas = *actual.Spec.Replicas
	}

	// No update pending: all pods already at target revision.
	if actual.Status.UpdatedReplicas >= replicas {
		return nil
	}

	// UpdatedReplicas is also short while a pod is missing: during a scale-up, or while a pod that was evicted
	// or could not be created is still on its way. That is not a rollout and must not hold the reconcile, so
	// what counts is whether any pod runs an older revision. CurrentRevision is not compared with
	// UpdateRevision to tell: under OnDelete the StatefulSet controller never advances it, and a StatefulSet
	// that was created again starts with both equal while its adopted pods can still run an older revision.
	outdated, err := be.findNextPodToUpdate(ctx, src, stsName, actual.Status.UpdateRevision)
	if err != nil {
		return err
	}
	if outdated == nil {
		return nil
	}

	// Wait for all existing pods to be ready before deleting the next one.
	if actual.Status.ReadyReplicas < replicas {
		logger.Info("waiting for all BE pods to be ready",
			"readyReplicas", actual.Status.ReadyReplicas,
			"replicas", replicas)
		return &subc.RequeueAfterError{
			After:  tabletBalanceCheckInterval,
			Reason: "waiting for BE pods to be ready",
		}
	}

	// Check tablet balance via FE.
	balanced, err := be.checkTabletBalance(ctx, src.Namespace, stsName)
	if err != nil {
		logger.Error(err, "tablet balance check failed, holding rollout")
		return &subc.RequeueAfterError{
			After:  tabletBalanceCheckInterval,
			Reason: "tablet balance check failed, will retry",
		}
	}
	if !balanced {
		logger.Info("tablets not balanced, holding BE rollout")
		return &subc.RequeueAfterError{
			After:  tabletBalanceCheckInterval,
			Reason: "waiting for tablet balance before next BE pod restart",
		}
	}

	// Find the next pod to delete: highest ordinal still at the old revision.
	podToDelete, err := be.findNextPodToUpdate(
		ctx, src, stsName, actual.Status.UpdateRevision)
	if err != nil {
		return err
	}
	if podToDelete == nil {
		return nil
	}

	logger.Info("deleting BE pod for controlled rollout",
		"pod", podToDelete.Name,
		"targetRevision", actual.Status.UpdateRevision)

	return be.Client.Delete(ctx, podToDelete)
}

// findNextPodToUpdate lists BE pods and returns the one with the highest
// ordinal that is not yet at the target revision.
func (be *BeController) findNextPodToUpdate(
	ctx context.Context, src *srapi.StarRocksCluster,
	stsName, targetRevision string,
) (*corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := be.Client.List(ctx, podList,
		client.InNamespace(src.Namespace),
		client.MatchingLabels(
			load.Selector(src.Name, src.Spec.StarRocksBeSpec)),
	); err != nil {
		return nil, fmt.Errorf("list BE pods: %w", err)
	}

	var candidate *corev1.Pod
	highestOrdinal := int32(-1)

	prefix := stsName + "-"
	for i := range podList.Items {
		p := &podList.Items[i]
		if p.Labels[appsv1.ControllerRevisionHashLabelKey] == targetRevision {
			continue // already updated
		}
		ordinalStr := strings.TrimPrefix(p.Name, prefix)
		ordinal, err := strconv.ParseInt(ordinalStr, 10, 32)
		if err != nil {
			continue
		}
		if int32(ordinal) > highestOrdinal {
			highestOrdinal = int32(ordinal)
			candidate = p
		}
	}

	return candidate, nil
}

// checkTabletBalance connects to the FE via MySQL and checks if tablet
// rebalancing is complete. Returns true when both pending_tablets and
// running_tablets are 0.
func (be *BeController) checkTabletBalance(
	ctx context.Context, namespace, stsName string,
) (bool, error) {
	logger := logr.FromContextOrDiscard(ctx)

	var sts appsv1.StatefulSet
	if err := be.Client.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      stsName,
	}, &sts); err != nil {
		return false, fmt.Errorf("get BE statefulset: %w", err)
	}

	rootPassword, feServiceName, feServicePort, err :=
		be.extractFEConnectionInfo(ctx, namespace, &sts)
	if err != nil {
		return false, err
	}

	if !strings.Contains(feServiceName, ".") {
		feServiceName = feServiceName + "." + namespace
	}

	dsn := fmt.Sprintf("root:%s@tcp(%s:%s)/",
		rootPassword, feServiceName, feServicePort)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return false, fmt.Errorf("open mysql connection: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, "SHOW PROC '/cluster_balance'")
	if err != nil {
		return false, fmt.Errorf("query cluster_balance: %w", err)
	}
	defer rows.Close()

	var pendingTablets, runningTablets int64
	for rows.Next() {
		var item, number string
		if err := rows.Scan(&item, &number); err != nil {
			return false, fmt.Errorf("scan row: %w", err)
		}
		switch item {
		case "pending_tablets":
			pendingTablets, _ = strconv.ParseInt(number, 10, 64)
		case "running_tablets":
			runningTablets, _ = strconv.ParseInt(number, 10, 64)
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate rows: %w", err)
	}

	logger.Info("tablet balance status",
		"pendingTablets", pendingTablets,
		"runningTablets", runningTablets)

	return pendingTablets == 0 && runningTablets == 0, nil
}

// extractFEConnectionInfo reads FE_SERVICE_NAME, FE_QUERY_PORT, and
// MYSQL_PWD from the BE StatefulSet's container env vars.
func (be *BeController) extractFEConnectionInfo(
	ctx context.Context, namespace string, sts *appsv1.StatefulSet,
) (rootPassword, feServiceName, feServicePort string, err error) {
	for _, envVar := range sts.Spec.Template.Spec.Containers[0].Env {
		switch envVar.Name {
		case "MYSQL_PWD":
			rootPassword, _ = k8sutils.GetEnvVarValue(
				ctx, be.Client, namespace, envVar)
		case "FE_SERVICE_NAME":
			feServiceName, err = k8sutils.GetEnvVarValue(
				ctx, be.Client, namespace, envVar)
			if err != nil {
				return "", "", "",
					fmt.Errorf("get FE_SERVICE_NAME: %w", err)
			}
		case "FE_QUERY_PORT":
			feServicePort, err = k8sutils.GetEnvVarValue(
				ctx, be.Client, namespace, envVar)
			if err != nil {
				return "", "", "",
					fmt.Errorf("get FE_QUERY_PORT: %w", err)
			}
		}
	}

	if feServiceName == "" || feServicePort == "" {
		return "", "", "", fmt.Errorf(
			"FE_SERVICE_NAME or FE_QUERY_PORT not found in BE env vars")
	}

	return rootPassword, feServiceName, feServicePort, nil
}
