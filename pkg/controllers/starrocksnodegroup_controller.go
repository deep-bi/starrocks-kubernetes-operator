package controllers

import (
	"context"
	"errors"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	srapi "github.com/StarRocks/starrocks-kubernetes-operator/pkg/apis/starrocks/v1"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/subcontrollers/nodegroup"
)

// StarRocksNodeGroupReconciler reconciles a StarRocksNodeGroup object
type StarRocksNodeGroupReconciler struct {
	client.Client
	controller *nodegroup.Controller
	denyList   string
}

// +kubebuilder:rbac:groups=starrocks.com,resources=starrocksnodegroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=starrocks.com,resources=starrocksnodegroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=starrocks.com,resources=starrocksnodegroups/finalizers,verbs=update

// Reconcile moves a StarRocksNodeGroup towards its spec.
func (r *StarRocksNodeGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logr.FromContextOrDiscard(ctx).WithName("StarRocksNodeGroupReconciler").
		WithValues("name", req.Name, "namespace", req.Namespace)
	ctx = logr.NewContext(ctx, logger)

	group := &srapi.StarRocksNodeGroup{}
	if err := r.Get(ctx, req.NamespacedName, group); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !group.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, group)
	}

	if !controllerutil.ContainsFinalizer(group, srapi.NodeGroupFinalizer) {
		controllerutil.AddFinalizer(group, srapi.NodeGroupFinalizer)
		// the update is seen as an event, which reconciles the group
		return ctrl.Result{}, r.Update(ctx, group)
	}

	logger.Info("begin to reconcile StarRocksNodeGroup")
	requeueAfter, syncErr := r.controller.Sync(ctx, group)
	if err := r.updateStatus(ctx, group); err != nil {
		logger.Error(err, "update StarRocksNodeGroup status failed")
		return ctrl.Result{}, err
	}
	if syncErr != nil {
		return ctrl.Result{}, syncErr
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *StarRocksNodeGroupReconciler) finalize(ctx context.Context, group *srapi.StarRocksNodeGroup) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(group, srapi.NodeGroupFinalizer) {
		return ctrl.Result{}, nil
	}
	if err := r.controller.Finalize(ctx, group); err != nil {
		if errors.Is(err, nodegroup.ErrPodsStillRunning) {
			return ctrl.Result{RequeueAfter: nodegroup.RequeueInterval / 5}, nil
		}
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(group, srapi.NodeGroupFinalizer)
	return ctrl.Result{}, r.Update(ctx, group)
}

// updateStatus writes the status of the group when it changed.
func (r *StarRocksNodeGroupReconciler) updateStatus(ctx context.Context, group *srapi.StarRocksNodeGroup) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		actual := &srapi.StarRocksNodeGroup{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: group.Namespace, Name: group.Name}, actual); err != nil {
			return client.IgnoreNotFound(err)
		}
		if equality.Semantic.DeepEqual(actual.Status, group.Status) {
			return nil
		}
		actual.Status = group.Status
		return r.Status().Update(ctx, actual)
	})
}

// groupsOfCluster maps a StarRocksCluster to the node groups that belong to it, so that a change of the
// cluster (its image, its FE becoming ready, the end of a disaster recovery) reaches every group.
func (r *StarRocksNodeGroupReconciler) groupsOfCluster(object client.Object) []reconcile.Request {
	var groups srapi.StarRocksNodeGroupList
	if err := r.List(context.Background(), &groups, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range groups.Items {
		if groups.Items[i].Spec.StarRocksCluster == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: groups.Items[i].Namespace,
				Name:      groups.Items[i].Name,
			}})
		}
	}
	return requests
}
