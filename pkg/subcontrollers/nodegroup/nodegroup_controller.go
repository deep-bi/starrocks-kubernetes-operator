// Package nodegroup reconciles StarRocksNodeGroup: a named set of identical CN or FE observer pods that
// belongs to one StarRocksCluster.
package nodegroup

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	srapi "github.com/StarRocks/starrocks-kubernetes-operator/pkg/apis/starrocks/v1"
	rutils "github.com/StarRocks/starrocks-kubernetes-operator/pkg/common/resource_utils"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/k8sutils"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/k8sutils/load"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/k8sutils/templates/object"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/k8sutils/templates/pod"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/k8sutils/templates/service"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/k8sutils/templates/statefulset"
	subc "github.com/StarRocks/starrocks-kubernetes-operator/pkg/subcontrollers"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/subcontrollers/cn"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/subcontrollers/fe"
)

const (
	mysqlPasswordEnv = "MYSQL_PWD" //nolint:gosec // the name of an environment variable, not a credential

	// RequeueInterval is how long the controller waits before it looks again at a group that waits for
	// something no watch reports: the FE becoming ready, or a node registering with the FE.
	RequeueInterval = 10 * time.Second
)

// DropNodesTimeout is how long a deleted group keeps trying to drop its nodes from the FE.
var DropNodesTimeout = 2 * time.Minute

// Reasons of the conditions of a node group.
const (
	ReasonClusterNotFound    = "ClusterNotFound"
	ReasonDisasterRecovery   = "DisasterRecovery"
	ReasonFeNotReady         = "FeNotReady"
	ReasonFeReady            = "FeReady"
	ReasonInvalidSpec        = "InvalidSpec"
	ReasonSyncFailed         = "SyncFailed"
	ReasonPodsNotReady       = "PodsNotReady"
	ReasonPodsFailing        = "PodsFailing"
	ReasonAllPodsReady       = "AllPodsReady"
	ReasonFeUnreachable      = "FeUnreachable"
	ReasonNodesNotRegistered = "NodesNotRegistered"
	ReasonStaleNodes         = "StaleNodes"
	ReasonAllNodesAlive      = "AllNodesAlive"
	ReasonRolloutInProgress  = "RolloutInProgress"
	ReasonAllNodesServing    = "AllNodesServing"
)

// Controller reconciles one StarRocksNodeGroup.
type Controller struct {
	Client   client.Client
	Recorder record.EventRecorder

	// NewFEClient builds the client the FE is asked through. Tests replace it.
	NewFEClient NewFEClientFunc

	cn *cn.CnController
}

func New(k8sClient client.Client, recorderFor subc.GetEventRecorderForFunc) *Controller {
	return &Controller{
		Client:      k8sClient,
		Recorder:    recorderFor("nodeGroupController"),
		NewFEClient: NewMySQLFEClient,
		cn:          cn.New(k8sClient, recorderFor),
	}
}

// component is a node group resolved against its cluster: the spec of the StarRocks component it runs.
type component struct {
	object object.StarRocksObject
	role   srapi.NodeGroupRole
	// spec is cnSpec or feSpec, whichever the role calls for.
	spec    srapi.SpecInterface
	cnSpec  *srapi.StarRocksCnSpec
	feSpec  *srapi.StarRocksFeSpec
	image   string
	envVars []corev1.EnvVar
}

func (c *component) statefulSetName() string {
	return load.Name(c.object.SubResourcePrefixName, c.spec)
}

// emptySpec returns a typed nil spec of the role, which is all the naming helpers need.
func emptySpec(role srapi.NodeGroupRole) srapi.SpecInterface {
	if role == srapi.NodeGroupRoleFEObserver {
		return (*srapi.StarRocksFeSpec)(nil)
	}
	return (*srapi.StarRocksCnSpec)(nil)
}

// StatefulSetName is the name of the StatefulSet that runs the pods of a group.
func StatefulSetName(group *srapi.StarRocksNodeGroup) string {
	return load.Name(group.Name, emptySpec(group.Spec.Role))
}

// resolve fills in what a group takes from its cluster: the image, and the ConfigMap and root password when
// the group names none of its own.
func resolve(cluster *srapi.StarRocksCluster, group *srapi.StarRocksNodeGroup) (*component, error) {
	image, err := ResolveImage(cluster, group)
	if err != nil {
		return nil, err
	}

	template := group.Spec.Template.DeepCopy()
	var clusterConfig srapi.ConfigMapInfo
	switch group.Spec.Role {
	case srapi.NodeGroupRoleFEObserver:
		clusterConfig = cluster.Spec.StarRocksFeSpec.ConfigMapInfo
	case srapi.NodeGroupRoleCN:
		if cluster.Spec.StarRocksCnSpec != nil {
			clusterConfig = cluster.Spec.StarRocksCnSpec.ConfigMapInfo
		}
	}
	if template.ConfigMapInfo.ConfigMapName == "" {
		template.ConfigMapInfo = clusterConfig
	}
	if !hasEnv(template.EnvVars, mysqlPasswordEnv) {
		if password := clusterEnv(cluster, group.Spec.Role, mysqlPasswordEnv); password != nil {
			template.EnvVars = append(template.EnvVars, *password)
		}
	}

	comp := &component{
		object: object.NewFromNodeGroup(group),
		role:   group.Spec.Role,
		image:  image,
	}
	if group.Spec.Role == srapi.NodeGroupRoleFEObserver {
		envVars := make([]corev1.EnvVar, 0, len(template.EnvVars)+1)
		for _, envVar := range template.EnvVars {
			if envVar.Name != srapi.EnvIsFEObserver {
				envVars = append(envVars, envVar)
			}
		}
		template.EnvVars = append(envVars, corev1.EnvVar{Name: srapi.EnvIsFEObserver, Value: "true"})
		comp.feSpec = template.ToFeSpec(image)
		comp.spec = comp.feSpec
	} else {
		comp.cnSpec = template.ToCnSpec(image)
		comp.spec = comp.cnSpec
	}
	comp.envVars = template.EnvVars
	return comp, nil
}

func hasEnv(envVars []corev1.EnvVar, name string) bool {
	for i := range envVars {
		if envVars[i].Name == name {
			return true
		}
	}
	return false
}

// clusterEnv returns an environment variable of the cluster, looked up on the component of the role first
// and on the other components after it.
func clusterEnv(cluster *srapi.StarRocksCluster, role srapi.NodeGroupRole, name string) *corev1.EnvVar {
	var fe, cn, be []corev1.EnvVar
	if cluster.Spec.StarRocksFeSpec != nil {
		fe = cluster.Spec.StarRocksFeSpec.FeEnvVars
	}
	if cluster.Spec.StarRocksCnSpec != nil {
		cn = cluster.Spec.StarRocksCnSpec.CnEnvVars
	}
	if cluster.Spec.StarRocksBeSpec != nil {
		be = cluster.Spec.StarRocksBeSpec.BeEnvVars
	}
	order := [][]corev1.EnvVar{cn, fe, be}
	if role == srapi.NodeGroupRoleFEObserver {
		order = [][]corev1.EnvVar{fe, cn, be}
	}
	for _, envVars := range order {
		for i := range envVars {
			if envVars[i].Name == name {
				return envVars[i].DeepCopy()
			}
		}
	}
	return nil
}

// Sync moves a node group towards its spec and writes what it finds to group.Status. It returns how long to
// wait before looking again; zero means a watch will report the next change.
func (c *Controller) Sync(ctx context.Context, group *srapi.StarRocksNodeGroup) (time.Duration, error) {
	logger := logr.FromContextOrDiscard(ctx)
	group.Status.ObservedGeneration = group.Generation

	cluster := &srapi.StarRocksCluster{}
	err := c.Client.Get(ctx, types.NamespacedName{Namespace: group.Namespace, Name: group.Spec.StarRocksCluster}, cluster)
	if apierrors.IsNotFound(err) {
		setCondition(group, srapi.NodeGroupClusterReady, false, ReasonClusterNotFound,
			fmt.Sprintf("StarRocksCluster %s not found in namespace %s", group.Spec.StarRocksCluster, group.Namespace))
		return 0, c.observe(ctx, group, nil, nil, nil)
	} else if err != nil {
		return 0, err
	}

	comp, err := resolve(cluster, group)
	if err != nil {
		c.Recorder.Event(group, corev1.EventTypeWarning, ReasonInvalidSpec, err.Error())
		setCondition(group, srapi.NodeGroupClusterReady, false, ReasonInvalidSpec, err.Error())
		return 0, c.observe(ctx, group, nil, nil, nil)
	}
	group.Status.Image = comp.image

	feConfig, err := fe.GetFEConfig(ctx, c.Client, cluster.Spec.StarRocksFeSpec, cluster.Namespace)
	if err != nil {
		return 0, err
	}

	requeue := time.Duration(0)
	reason, message := clusterGate(ctx, c.Client, cluster, feConfig)
	if reason == ReasonFeReady && c.upgradeWaitsForFE(ctx, cluster, comp) {
		reason, message = ReasonFeNotReady, "the FE of the cluster has not finished rolling out its new image"
	}
	switch reason {
	case ReasonFeReady:
		setCondition(group, srapi.NodeGroupClusterReady, true, ReasonFeReady, "")
		if err = c.syncComponent(ctx, cluster, group, comp); err != nil {
			logger.Error(err, "sync node group failed")
			c.Recorder.Event(group, corev1.EventTypeWarning, ReasonSyncFailed, err.Error())
			if observeErr := c.observe(ctx, group, cluster, comp, feConfig); observeErr != nil {
				logger.Error(observeErr, "observe node group failed")
			}
			setCondition(group, srapi.NodeGroupReady, false, ReasonSyncFailed, err.Error())
			group.Status.Phase = srapi.ComponentFailed
			return 0, err
		}
	default:
		logger.Info("the cluster is not ready, the node group changes nothing", "reason", reason, "message", message)
		setCondition(group, srapi.NodeGroupClusterReady, false, reason, message)
		requeue = RequeueInterval
	}

	if err = c.observe(ctx, group, cluster, comp, feConfig); err != nil {
		return 0, err
	}
	if requeue == 0 && !meta.IsStatusConditionTrue(group.Status.Conditions, srapi.NodeGroupRegistered) {
		// nothing tells the operator when a node registers with the FE
		requeue = RequeueInterval
	}
	return requeue, nil
}

// clusterGate decides whether a node group may change anything. It returns ReasonFeReady when it may.
func clusterGate(ctx context.Context, k8sClient client.Client, cluster *srapi.StarRocksCluster,
	feConfig map[string]interface{}) (reason, message string) {
	// While the cluster is restored from a snapshot the FE runs alone and rebuilds its list of nodes. A node
	// that registers now would be lost or would confuse the restore, as it would for the cluster's own BE and CN.
	if dr, _ := fe.ShouldEnterDisasterRecoveryMode(cluster.Spec.DisasterRecovery,
		cluster.Status.DisasterRecoveryStatus, feConfig); dr {
		return ReasonDisasterRecovery, "the cluster is in disaster recovery"
	}
	if cluster.Spec.WaitForFullRollout {
		if !fe.CheckFEFullyRolledOut(ctx, k8sClient, cluster.Namespace, cluster.Name) {
			return ReasonFeNotReady, "the FE of the cluster is not fully rolled out"
		}
	} else if !fe.CheckFEReady(ctx, k8sClient, cluster.Namespace, cluster.Name) {
		return ReasonFeNotReady, "the FE of the cluster is not ready"
	}
	return ReasonFeReady, ""
}

// upgradeWaitsForFE reports whether the group is about to change its image while the FE of the cluster has
// not finished changing its own. The FE goes first, as it does for the BE and CN of the cluster.
//
// A change of the cluster reaches this controller and the cluster controller at the same time, so the FE
// StatefulSet can still describe the old image, fully rolled out. Its image is therefore compared with the
// spec of the cluster before its status is trusted.
func (c *Controller) upgradeWaitsForFE(ctx context.Context, cluster *srapi.StarRocksCluster, comp *component) bool {
	var sts appsv1.StatefulSet
	key := types.NamespacedName{Namespace: comp.object.Namespace, Name: comp.statefulSetName()}
	if err := c.Client.Get(ctx, key, &sts); err != nil || len(sts.Spec.Template.Spec.Containers) == 0 {
		return false
	}
	if sts.Spec.Template.Spec.Containers[0].Image == comp.image {
		return false
	}

	var feSts appsv1.StatefulSet
	key = types.NamespacedName{Namespace: cluster.Namespace, Name: load.Name(cluster.Name, cluster.Spec.StarRocksFeSpec)}
	if err := c.Client.Get(ctx, key, &feSts); err != nil || len(feSts.Spec.Template.Spec.Containers) == 0 {
		return true
	}
	if feSts.Spec.Template.Spec.Containers[0].Image != cluster.Spec.StarRocksFeSpec.Image {
		return true
	}
	return !fe.CheckFEFullyRolledOut(ctx, c.Client, cluster.Namespace, cluster.Name)
}

func (c *Controller) syncComponent(ctx context.Context, cluster *srapi.StarRocksCluster,
	group *srapi.StarRocksNodeGroup, comp *component) error {
	// The StatefulSet is named after the group. Another object can already use that name: the cluster "spot"
	// runs "spot-cn" as well. The group must not take such a StatefulSet over.
	var existing appsv1.StatefulSet
	err := c.Client.Get(ctx, types.NamespacedName{Namespace: group.Namespace, Name: comp.statefulSetName()}, &existing)
	if err == nil && !metav1.IsControlledBy(&existing, group) {
		return fmt.Errorf("statefulset %s already exists and does not belong to this node group: rename the node group",
			existing.Name)
	} else if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if comp.role == srapi.NodeGroupRoleFEObserver {
		return c.syncObservers(ctx, cluster, comp)
	}
	// A CN group is a CN component like the one of the cluster: same pods, same services, and the same rule
	// for dropping compute nodes from the FE on scale-in.
	return c.cn.SyncCnSpec(ctx, comp.object, comp.cnSpec, nil)
}

// syncObservers deploys the StatefulSet and the Services of a group of FE observers.
func (c *Controller) syncObservers(ctx context.Context, cluster *srapi.StarRocksCluster, comp *component) error {
	logger := logr.FromContextOrDiscard(ctx)
	feSpec := comp.feSpec
	namespace := comp.object.Namespace
	prefix := comp.object.SubResourcePrefixName

	if err := (&fe.FeController{}).Validating(feSpec); err != nil {
		return err
	}
	config, err := fe.GetFEConfig(ctx, c.Client, feSpec, namespace)
	if err != nil {
		return err
	}

	podTemplate, err := fe.BuildPodTemplate(prefix, namespace, cluster.Name, feSpec, config)
	if err != nil {
		return err
	}
	// The FE service of the cluster selects on this label once the cluster asks for it, see
	// FEServiceIncludesObserversAnnotation. The observers always carry it, so that asking does not restart them.
	podTemplate.Labels[srapi.FEServiceLabelKey] = cluster.Name
	expect := statefulset.MakeStatefulset(comp.object, feSpec, podTemplate)
	if err = k8sutils.ApplyStatefulSet(ctx, c.Client, &expect, true, rutils.StatefulSetDeepEqual); err != nil {
		return err
	}
	if err = k8sutils.PatchPVCVolumeAttributesClass(ctx, c.Client, namespace, expect.Name, feSpec.StorageVolumes); err != nil {
		logger.Error(err, "patch PVC volumeAttributesClassName failed")
	}

	labels := load.Labels(prefix, feSpec)
	external := rutils.BuildExternalService(comp.object, feSpec, config, load.Selector(prefix, feSpec), labels)
	queryPort := rutils.GetPort(config, rutils.QUERY_PORT)
	mysql := "mysql"
	search := service.MakeSearchService(service.SearchServiceName(prefix, feSpec), &external, []corev1.ServicePort{
		{Name: "query-port", Port: queryPort, TargetPort: intstr.FromInt(int(queryPort)), AppProtocol: &mysql},
	}, labels)
	if err = k8sutils.ApplyService(ctx, c.Client, search, rutils.ServiceDeepEqual); err != nil {
		return err
	}
	return k8sutils.ApplyService(ctx, c.Client, &external, rutils.ServiceDeepEqual)
}

// groupNode is a node the FE lists that is a pod of the group.
type groupNode struct {
	Node
	ordinal int32
}

// nodesOfGroup keeps the nodes whose host is a pod of the StatefulSet. A node registered by anything else
// (another StatefulSet, a node added by hand, a node registered by IP) is never counted and never dropped.
func nodesOfGroup(nodes []Node, statefulSetName string) []groupNode {
	var result []groupNode
	for _, node := range nodes {
		podName := strings.Split(node.Host, ".")[0]
		suffix := strings.TrimPrefix(podName, statefulSetName+"-")
		ordinal, err := strconv.ParseInt(suffix, 10, 32)
		if err != nil || podName != fmt.Sprintf("%s-%d", statefulSetName, ordinal) {
			continue
		}
		result = append(result, groupNode{Node: node, ordinal: int32(ordinal)})
	}
	return result
}

func desiredReplicas(group *srapi.StarRocksNodeGroup) int32 {
	if group.Spec.Template.Replicas == nil {
		return 1
	}
	return *group.Spec.Template.Replicas
}

func (c *Controller) listPods(ctx context.Context, group *srapi.StarRocksNodeGroup) (corev1.PodList, error) {
	var pods corev1.PodList
	err := c.Client.List(ctx, &pods, client.InNamespace(group.Namespace),
		client.MatchingLabels(load.Selector(group.Name, emptySpec(group.Spec.Role))))
	return pods, err
}

// observe writes the status of the group. cluster, comp and feConfig are nil when the cluster is missing or
// the spec is invalid: the pods are still reported then, the FE is not asked.
//
//nolint:gocyclo
func (c *Controller) observe(ctx context.Context, group *srapi.StarRocksNodeGroup, cluster *srapi.StarRocksCluster,
	comp *component, feConfig map[string]interface{}) error {
	logger := logr.FromContextOrDiscard(ctx)
	status := &group.Status
	desired := desiredReplicas(group)
	stsName := StatefulSetName(group)

	var sts appsv1.StatefulSet
	rolledOut := false
	err := c.Client.Get(ctx, types.NamespacedName{Namespace: group.Namespace, Name: stsName}, &sts)
	// the name is taken by another object: its pods and its nodes are not the group's
	foreign := err == nil && !metav1.IsControlledBy(&sts, group)
	switch {
	case apierrors.IsNotFound(err) || foreign:
		status.Replicas, status.ReadyReplicas, status.UpdatedReplicas = 0, 0, 0
	case err != nil:
		return err
	default:
		status.Replicas = sts.Status.Replicas
		status.ReadyReplicas = sts.Status.ReadyReplicas
		status.UpdatedReplicas = sts.Status.UpdatedReplicas
		selector, selectorErr := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
		if selectorErr != nil {
			return selectorErr
		}
		status.Selector = selector.String()
		rolledOut = sts.Status.ObservedGeneration >= sts.Generation && sts.DeletionTimestamp == nil &&
			sts.Spec.Replicas != nil && *sts.Spec.Replicas == desired &&
			status.Replicas == desired && status.UpdatedReplicas == desired
	}

	var pods corev1.PodList
	if !foreign {
		if pods, err = c.listPods(ctx, group); err != nil {
			return err
		}
	}
	_, _, failed := pod.Count(pods)
	switch {
	case len(failed) > 0:
		setCondition(group, srapi.NodeGroupAvailable, false, ReasonPodsFailing,
			fmt.Sprintf("pod %s is failing: %s", failed[0], pod.Status(pods)[failed[0]].Reason))
	case status.ReadyReplicas == desired && status.Replicas == desired:
		setCondition(group, srapi.NodeGroupAvailable, true, ReasonAllPodsReady, "")
	default:
		setCondition(group, srapi.NodeGroupAvailable, false, ReasonPodsNotReady,
			fmt.Sprintf("%d of %d pods are ready", status.ReadyReplicas, desired))
	}

	clusterReady := meta.IsStatusConditionTrue(status.Conditions, srapi.NodeGroupClusterReady)
	switch {
	case !clusterReady || cluster == nil || comp == nil:
		gate := meta.FindStatusCondition(status.Conditions, srapi.NodeGroupClusterReady)
		setCondition(group, srapi.NodeGroupRegistered, false, gate.Reason, "the FE is not asked while the cluster is not ready")
	case foreign:
		status.RegisteredReplicas = 0
		setCondition(group, srapi.NodeGroupRegistered, false, ReasonNodesNotRegistered, "the node group runs no pods")
	default:
		feClient := c.NewFEClient(ctx, c.Client, cluster, comp.envVars, feConfig)
		nodes, nodesErr := feClient.Nodes(ctx, group.Spec.Role)
		if nodesErr != nil {
			logger.Info("list the nodes of the group in FE failed", "error", nodesErr)
			setCondition(group, srapi.NodeGroupRegistered, false, ReasonFeUnreachable, nodesErr.Error())
		} else {
			mine := nodesOfGroup(nodes, stsName)
			// Observers are dropped here; compute nodes are dropped by the CN controller during its sync.
			if group.Spec.Role == srapi.NodeGroupRoleFEObserver {
				mine = c.dropGoneNodes(ctx, feClient, group, mine, pods, desired)
			}
			var alive, stale int32
			for _, node := range mine {
				switch {
				case node.ordinal >= desired:
					stale++
				case node.Alive:
					alive++
				}
			}
			status.RegisteredReplicas = alive
			switch {
			case stale > 0:
				setCondition(group, srapi.NodeGroupRegistered, false, ReasonStaleNodes,
					fmt.Sprintf("the FE still lists %d nodes of the group beyond its %d replicas", stale, desired))
			case alive != desired:
				setCondition(group, srapi.NodeGroupRegistered, false, ReasonNodesNotRegistered,
					fmt.Sprintf("the FE lists %d of %d nodes as alive", alive, desired))
			default:
				setCondition(group, srapi.NodeGroupRegistered, true, ReasonAllNodesAlive, "")
			}
		}
	}

	summarize(group, rolledOut)
	return nil
}

// dropGoneNodes drops from the FE the nodes of the group beyond its replicas whose pod no longer exists, and
// returns the nodes that are left. A node whose pod still runs is kept: it is dropped once the pod is gone.
func (c *Controller) dropGoneNodes(ctx context.Context, feClient FEClient, group *srapi.StarRocksNodeGroup,
	nodes []groupNode, pods corev1.PodList, desired int32) []groupNode {
	logger := logr.FromContextOrDiscard(ctx)
	running := make(map[string]bool, len(pods.Items))
	for i := range pods.Items {
		running[pods.Items[i].Name] = true
	}

	var left []groupNode
	for _, node := range nodes {
		podName := strings.Split(node.Host, ".")[0]
		if node.ordinal < desired || running[podName] {
			left = append(left, node)
			continue
		}
		if err := feClient.Drop(ctx, group.Spec.Role, node.Node); err != nil {
			logger.Error(err, "drop node from FE failed", "node", node.Host)
			left = append(left, node)
			continue
		}
		logger.Info("dropped node from FE", "node", node.Host, "role", group.Spec.Role)
		c.Recorder.Eventf(group, corev1.EventTypeNormal, "NodeDropped", "dropped %s from the FE", node.Host)
	}
	return left
}

// summarize derives the Ready condition and the phase from the other three conditions.
func summarize(group *srapi.StarRocksNodeGroup, rolledOut bool) {
	status := &group.Status
	status.Phase = srapi.ComponentReconciling

	for _, conditionType := range []string{srapi.NodeGroupClusterReady, srapi.NodeGroupAvailable, srapi.NodeGroupRegistered} {
		condition := meta.FindStatusCondition(status.Conditions, conditionType)
		if condition.Status == metav1.ConditionTrue {
			continue
		}
		setCondition(group, srapi.NodeGroupReady, false, condition.Reason, condition.Message)
		switch condition.Reason {
		case ReasonClusterNotFound, ReasonInvalidSpec, ReasonPodsFailing:
			status.Phase = srapi.ComponentFailed
		}
		return
	}
	if !rolledOut {
		setCondition(group, srapi.NodeGroupReady, false, ReasonRolloutInProgress,
			fmt.Sprintf("%d of %d pods run the current spec", status.UpdatedReplicas, desiredReplicas(group)))
		return
	}
	setCondition(group, srapi.NodeGroupReady, true, ReasonAllNodesServing, "")
	status.Phase = srapi.ComponentRunning
}

func setCondition(group *srapi.StarRocksNodeGroup, conditionType string, status bool, reason, message string) {
	conditionStatus := metav1.ConditionFalse
	if status {
		conditionStatus = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: group.Generation,
	})
}

// ErrPodsStillRunning is returned by Finalize while the pods of a deleted group are still terminating.
var ErrPodsStillRunning = errors.New("the pods of the node group are still terminating")

// Finalize cleans up after a node group that is being deleted: it removes the StatefulSet, waits for the pods
// to be gone and then drops their nodes from the FE. It returns ErrPodsStillRunning until the pods are gone.
//
// Dropping the nodes is tried for DropNodesTimeout and then given up. A group must not stay in Terminating
// because the FE is down or the cluster was deleted along with it; what is left in the FE then are dead
// nodes, which serve nothing.
func (c *Controller) Finalize(ctx context.Context, group *srapi.StarRocksNodeGroup) error {
	logger := logr.FromContextOrDiscard(ctx)
	stsName := StatefulSetName(group)

	var sts appsv1.StatefulSet
	err := c.Client.Get(ctx, types.NamespacedName{Namespace: group.Namespace, Name: stsName}, &sts)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil {
		if !metav1.IsControlledBy(&sts, group) {
			// The name was taken by another object, so the group never ran a pod. The pods and the nodes that
			// carry this name are that object's.
			return nil
		}
		if sts.DeletionTimestamp == nil {
			logger.Info("delete the statefulset of the node group", "statefulset", stsName)
			if err = c.Client.Delete(ctx, &sts); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
		return ErrPodsStillRunning
	}

	pods, err := c.listPods(ctx, group)
	if err != nil {
		return err
	}
	if len(pods.Items) > 0 {
		return ErrPodsStillRunning
	}

	if object.SkipsFECleanup(group.Annotations) {
		logger.Info("FE cleanup skipped by annotation, the nodes stay in the FE", "annotation", object.SkipFECleanupAnnotation)
		return nil
	}
	if err = c.dropAllNodes(ctx, group); err != nil {
		// The FE can be slow to answer right after it lost nodes, so the cleanup is tried again for a while.
		if group.DeletionTimestamp != nil && time.Since(group.DeletionTimestamp.Time) < DropNodesTimeout {
			return fmt.Errorf("drop the nodes of the node group from FE: %w", err)
		}
		logger.Error(err, "drop the nodes of the node group from FE failed, they stay in the FE as dead nodes")
		c.Recorder.Event(group, corev1.EventTypeWarning, "DropNodesFailed", err.Error())
	}
	return nil
}

func (c *Controller) dropAllNodes(ctx context.Context, group *srapi.StarRocksNodeGroup) error {
	cluster := &srapi.StarRocksCluster{}
	err := c.Client.Get(ctx, types.NamespacedName{Namespace: group.Namespace, Name: group.Spec.StarRocksCluster}, cluster)
	if apierrors.IsNotFound(err) || (err == nil && !cluster.DeletionTimestamp.IsZero()) {
		// the nodes go with the cluster
		return nil
	} else if err != nil {
		return err
	}

	comp, err := resolve(cluster, group)
	if err != nil {
		return err
	}
	feConfig, err := fe.GetFEConfig(ctx, c.Client, cluster.Spec.StarRocksFeSpec, cluster.Namespace)
	if err != nil {
		return err
	}
	feClient := c.NewFEClient(ctx, c.Client, cluster, comp.envVars, feConfig)
	nodes, err := feClient.Nodes(ctx, group.Spec.Role)
	if err != nil {
		return err
	}

	var failures []error
	for _, node := range nodesOfGroup(nodes, StatefulSetName(group)) {
		if err = feClient.Drop(ctx, group.Spec.Role, node.Node); err != nil {
			failures = append(failures, fmt.Errorf("drop %s: %w", node.Host, err))
			continue
		}
		logr.FromContextOrDiscard(ctx).Info("dropped node from FE", "node", node.Host, "role", group.Spec.Role)
	}
	return errors.Join(failures...)
}
