package nodegroup

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"

	srapi "github.com/StarRocks/starrocks-kubernetes-operator/pkg/apis/starrocks/v1"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/k8sutils/fake"
)

const namespace = "default"

func TestMain(m *testing.M) {
	srapi.Register()
	os.Exit(m.Run())
}

func cluster() *srapi.StarRocksCluster {
	return &srapi.StarRocksCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "sr", Namespace: namespace},
		Spec: srapi.StarRocksClusterSpec{
			StarRocksFeSpec: &srapi.StarRocksFeSpec{
				StarRocksComponentSpec: srapi.StarRocksComponentSpec{
					StarRocksLoadSpec: srapi.StarRocksLoadSpec{
						Image:         "starrocks/fe-ubuntu:4.1.8",
						ConfigMapInfo: srapi.ConfigMapInfo{ConfigMapName: "fe-cm", ResolveKey: "fe.conf"},
					},
				},
				FeEnvVars: []corev1.EnvVar{{Name: "MYSQL_PWD", Value: "secret"}},
			},
			StarRocksCnSpec: &srapi.StarRocksCnSpec{
				StarRocksComponentSpec: srapi.StarRocksComponentSpec{
					StarRocksLoadSpec: srapi.StarRocksLoadSpec{
						Image:         "starrocks/cn-ubuntu:4.1.8",
						ConfigMapInfo: srapi.ConfigMapInfo{ConfigMapName: "cn-cm", ResolveKey: "cn.conf"},
					},
				},
			},
		},
	}
}

func group(name string, role srapi.NodeGroupRole, replicas int32) *srapi.StarRocksNodeGroup {
	return &srapi.StarRocksNodeGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(name), Generation: 3},
		Spec: srapi.StarRocksNodeGroupSpec{
			StarRocksCluster: "sr",
			Role:             role,
			Template: srapi.NodeGroupTemplate{
				StarRocksComponentSpec: srapi.StarRocksComponentSpec{
					StarRocksLoadSpec: srapi.StarRocksLoadSpec{Replicas: pointer.Int32(replicas)},
				},
			},
		},
	}
}

func configMaps() []runtime.Object {
	return []runtime.Object{
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "fe-cm", Namespace: namespace},
			Data:       map[string]string{"fe.conf": "query_port = 9030\n"},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "cn-cm", Namespace: namespace},
			Data:       map[string]string{"cn.conf": "sys_log_level = INFO\n"},
		},
	}
}

// feEndpoints makes the FE of the cluster ready.
func feEndpoints() *corev1.Endpoints {
	return &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "sr-fe-service", Namespace: namespace},
		Subsets:    []corev1.EndpointSubset{{Addresses: []corev1.EndpointAddress{{IP: "10.0.0.1"}}}},
	}
}

type fakeFE struct {
	nodes   []Node
	err     error
	dropped []string
}

func (f *fakeFE) Nodes(context.Context, srapi.NodeGroupRole) ([]Node, error) {
	return f.nodes, f.err
}

func (f *fakeFE) Drop(_ context.Context, _ srapi.NodeGroupRole, node Node) error {
	f.dropped = append(f.dropped, node.Host)
	return nil
}

func newController(feClient *fakeFE, objects ...runtime.Object) *Controller {
	controller := New(fake.NewFakeClient(srapi.Scheme, objects...), fake.GetEventRecorderFor(nil))
	controller.NewFEClient = func(context.Context, client.Client, *srapi.StarRocksCluster,
		[]corev1.EnvVar, map[string]interface{}) FEClient {
		return feClient
	}
	return controller
}

func condition(g *srapi.StarRocksNodeGroup, conditionType string) metav1.Condition {
	return *meta.FindStatusCondition(g.Status.Conditions, conditionType)
}

func getSts(t *testing.T, c *Controller, name string) *appsv1.StatefulSet {
	t.Helper()
	sts := &appsv1.StatefulSet{}
	require.NoError(t, c.Client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, sts))
	return sts
}

func TestResolveImage(t *testing.T) {
	tests := []struct {
		name       string
		role       srapi.NodeGroupRole
		repository string
		mutate     func(*srapi.StarRocksCluster)
		want       string
		wantErr    string
	}{
		{name: "cn takes the cluster's CN image", role: srapi.NodeGroupRoleCN, want: "starrocks/cn-ubuntu:4.1.8"},
		{name: "observer takes the cluster's FE image", role: srapi.NodeGroupRoleFEObserver, want: "starrocks/fe-ubuntu:4.1.8"},
		{
			name: "a repository gets the cluster's tag", role: srapi.NodeGroupRoleCN,
			repository: "registry.local:5000/sr/cn", want: "registry.local:5000/sr/cn:4.1.8",
		},
		{
			name: "a tag on the group is refused", role: srapi.NodeGroupRoleCN,
			repository: "starrocks/cn-ubuntu:4.0.0", wantErr: "must name a repository only",
		},
		{
			name: "a digest on the group is refused", role: srapi.NodeGroupRoleCN,
			repository: "starrocks/cn-ubuntu@sha256:abc", wantErr: "must name a repository only",
		},
		{
			name: "cn without a CN component needs a repository", role: srapi.NodeGroupRoleCN,
			mutate:  func(c *srapi.StarRocksCluster) { c.Spec.StarRocksCnSpec = nil },
			wantErr: "has no starRocksCnSpec",
		},
		{
			name: "cn without a CN component takes the FE tag", role: srapi.NodeGroupRoleCN, repository: "starrocks/cn-ubuntu",
			mutate: func(c *srapi.StarRocksCluster) { c.Spec.StarRocksCnSpec = nil },
			want:   "starrocks/cn-ubuntu:4.1.8",
		},
		{
			name: "a cluster pinned by digest has no tag to share", role: srapi.NodeGroupRoleFEObserver, repository: "mirror/fe",
			mutate:  func(c *srapi.StarRocksCluster) { c.Spec.StarRocksFeSpec.Image = "starrocks/fe-ubuntu@sha256:abc" },
			wantErr: "has no tag to share",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := cluster()
			if tt.mutate != nil {
				tt.mutate(src)
			}
			g := group("spot", tt.role, 1)
			g.Spec.Template.Image = tt.repository
			got, err := ResolveImage(src, g)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestResolveTakesConfigAndPasswordFromTheCluster(t *testing.T) {
	comp, err := resolve(cluster(), group("readers", srapi.NodeGroupRoleFEObserver, 1))
	require.NoError(t, err)
	require.Equal(t, "fe-cm", comp.feSpec.ConfigMapInfo.ConfigMapName)
	require.Contains(t, comp.feSpec.FeEnvVars, corev1.EnvVar{Name: "MYSQL_PWD", Value: "secret"})
	require.Contains(t, comp.feSpec.FeEnvVars, corev1.EnvVar{Name: srapi.EnvIsFEObserver, Value: "true"})

	// what the group sets wins, and nobody can turn an observer group into followers
	g := group("readers", srapi.NodeGroupRoleFEObserver, 1)
	g.Spec.Template.ConfigMapInfo = srapi.ConfigMapInfo{ConfigMapName: "own-cm", ResolveKey: "fe.conf"}
	g.Spec.Template.EnvVars = []corev1.EnvVar{
		{Name: "MYSQL_PWD", Value: "own"},
		{Name: srapi.EnvIsFEObserver, Value: "false"},
	}
	comp, err = resolve(cluster(), g)
	require.NoError(t, err)
	require.Equal(t, "own-cm", comp.feSpec.ConfigMapInfo.ConfigMapName)
	require.Equal(t, []corev1.EnvVar{
		{Name: "MYSQL_PWD", Value: "own"},
		{Name: srapi.EnvIsFEObserver, Value: "true"},
	}, comp.feSpec.FeEnvVars)

	comp, err = resolve(cluster(), group("spot", srapi.NodeGroupRoleCN, 1))
	require.NoError(t, err)
	require.Equal(t, "cn-cm", comp.cnSpec.ConfigMapInfo.ConfigMapName)
	require.Contains(t, comp.cnSpec.CnEnvVars, corev1.EnvVar{Name: "MYSQL_PWD", Value: "secret"})
}

func TestNodesOfGroup(t *testing.T) {
	nodes := []Node{
		{Host: "spot-cn-0.spot-cn-search.default.svc.cluster.local"},
		{Host: "spot-cn-11.spot-cn-search.default.svc.cluster.local"},
		{Host: "spot-cn-extra-0.spot-cn-extra-search.default.svc.cluster.local"}, // another group
		{Host: "sr-cn-0.sr-cn-search.default.svc.cluster.local"},                 // the cluster's own CN
		{Host: "10.0.0.7"}, // registered by hand
	}
	got := nodesOfGroup(nodes, "spot-cn")
	require.Len(t, got, 2)
	require.Equal(t, int32(0), got[0].ordinal)
	require.Equal(t, int32(11), got[1].ordinal)
}

func TestSyncWaitsForTheCluster(t *testing.T) {
	t.Run("cluster not found", func(t *testing.T) {
		g := group("spot", srapi.NodeGroupRoleCN, 2)
		c := newController(&fakeFE{}, g)
		_, err := c.Sync(context.Background(), g)
		require.NoError(t, err)
		require.Equal(t, ReasonClusterNotFound, condition(g, srapi.NodeGroupClusterReady).Reason)
		require.Equal(t, ReasonClusterNotFound, condition(g, srapi.NodeGroupReady).Reason)
		require.Equal(t, srapi.ComponentFailed, g.Status.Phase)
		require.Equal(t, int64(3), g.Status.ObservedGeneration)
	})

	t.Run("FE not ready", func(t *testing.T) {
		g := group("spot", srapi.NodeGroupRoleCN, 2)
		c := newController(&fakeFE{}, append(configMaps(), cluster(), g)...)
		requeue, err := c.Sync(context.Background(), g)
		require.NoError(t, err)
		require.Equal(t, RequeueInterval, requeue)
		require.Equal(t, ReasonFeNotReady, condition(g, srapi.NodeGroupClusterReady).Reason)
		require.Equal(t, srapi.ComponentReconciling, g.Status.Phase)
		require.Error(t, c.Client.Get(context.Background(),
			types.NamespacedName{Namespace: namespace, Name: "spot-cn"}, &appsv1.StatefulSet{}))
	})

	t.Run("cluster in disaster recovery", func(t *testing.T) {
		src := cluster()
		src.Spec.DisasterRecovery = &srapi.DisasterRecovery{Enabled: true, Generation: 1}
		g := group("spot", srapi.NodeGroupRoleCN, 2)
		objects := append(configMaps(), src, g, feEndpoints())
		objects[0].(*corev1.ConfigMap).Data["fe.conf"] = "run_mode = shared_data\n"
		c := newController(&fakeFE{}, objects...)
		_, err := c.Sync(context.Background(), g)
		require.NoError(t, err)
		require.Equal(t, ReasonDisasterRecovery, condition(g, srapi.NodeGroupClusterReady).Reason)
		require.Error(t, c.Client.Get(context.Background(),
			types.NamespacedName{Namespace: namespace, Name: "spot-cn"}, &appsv1.StatefulSet{}))
	})

	t.Run("a tag on the group", func(t *testing.T) {
		g := group("spot", srapi.NodeGroupRoleCN, 2)
		g.Spec.Template.Image = "starrocks/cn-ubuntu:3.5.0"
		c := newController(&fakeFE{}, append(configMaps(), cluster(), g, feEndpoints())...)
		_, err := c.Sync(context.Background(), g)
		require.NoError(t, err)
		require.Equal(t, ReasonInvalidSpec, condition(g, srapi.NodeGroupReady).Reason)
		require.Equal(t, srapi.ComponentFailed, g.Status.Phase)
	})
}

func TestSyncCnGroup(t *testing.T) {
	g := group("spot", srapi.NodeGroupRoleCN, 2)
	c := newController(&fakeFE{}, append(configMaps(), cluster(), g, feEndpoints())...)
	_, err := c.Sync(context.Background(), g)
	require.NoError(t, err)

	sts := getSts(t, c, "spot-cn")
	require.True(t, metav1.IsControlledBy(sts, g))
	require.Equal(t, int32(2), *sts.Spec.Replicas)
	container := sts.Spec.Template.Spec.Containers[0]
	require.Equal(t, "starrocks/cn-ubuntu:4.1.8", container.Image)
	require.Contains(t, container.Env, corev1.EnvVar{Name: "FE_SERVICE_NAME", Value: "sr-fe-service"})
	require.Contains(t, container.Env, corev1.EnvVar{Name: "MYSQL_PWD", Value: "secret"})
	require.Equal(t, "starrocks/cn-ubuntu:4.1.8", g.Status.Image)
	require.Equal(t, ReasonPodsNotReady, condition(g, srapi.NodeGroupReady).Reason)

	// the group follows the image of its cluster
	src := &srapi.StarRocksCluster{}
	require.NoError(t, c.Client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: "sr"}, src))
	src.Spec.StarRocksCnSpec.Image = "starrocks/cn-ubuntu:4.1.9"
	require.NoError(t, c.Client.Update(context.Background(), src))
	require.NoError(t, c.Client.Create(context.Background(), feSts("starrocks/fe-ubuntu:4.1.8", 3)))
	_, err = c.Sync(context.Background(), g)
	require.NoError(t, err)
	require.Equal(t, "starrocks/cn-ubuntu:4.1.9", getSts(t, c, "spot-cn").Spec.Template.Spec.Containers[0].Image)
}

// feSts is the FE StatefulSet of the cluster, running image on every pod.
func feSts(image string, updated int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sr-fe", Namespace: namespace},
		Spec: appsv1.StatefulSetSpec{
			Replicas: pointer.Int32(3),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fe", Image: image}}}},
		},
		Status: appsv1.StatefulSetStatus{
			Replicas: 3, ReadyReplicas: 3, UpdatedReplicas: updated, CurrentRevision: "a", UpdateRevision: "a",
		},
	}
}

func TestUpgradeWaitsForTheFE(t *testing.T) {
	const oldCN, newCN = "starrocks/cn-ubuntu:4.1.8", "starrocks/cn-ubuntu:4.1.9"
	tests := []struct {
		name      string
		feSts     *appsv1.StatefulSet
		wantImage string
	}{
		{name: "the FE statefulset still describes the old image", feSts: feSts("starrocks/fe-ubuntu:4.1.8", 3), wantImage: oldCN},
		{name: "the FE is rolling out the new image", feSts: feSts("starrocks/fe-ubuntu:4.1.9", 1), wantImage: oldCN},
		{name: "the FE runs the new image", feSts: feSts("starrocks/fe-ubuntu:4.1.9", 3), wantImage: newCN},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := group("spot", srapi.NodeGroupRoleCN, 2)
			c := newController(&fakeFE{}, append(configMaps(), cluster(), g, feEndpoints())...)
			_, err := c.Sync(context.Background(), g)
			require.NoError(t, err)

			src := &srapi.StarRocksCluster{}
			require.NoError(t, c.Client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: "sr"}, src))
			src.Spec.StarRocksFeSpec.Image = "starrocks/fe-ubuntu:4.1.9"
			src.Spec.StarRocksCnSpec.Image = newCN
			require.NoError(t, c.Client.Update(context.Background(), src))
			require.NoError(t, c.Client.Create(context.Background(), tt.feSts))

			_, err = c.Sync(context.Background(), g)
			require.NoError(t, err)
			require.Equal(t, tt.wantImage, getSts(t, c, "spot-cn").Spec.Template.Spec.Containers[0].Image)
			require.Equal(t, tt.wantImage == newCN, meta.IsStatusConditionTrue(g.Status.Conditions, srapi.NodeGroupClusterReady))
		})
	}
}

func TestSyncObserverGroup(t *testing.T) {
	g := group("readers", srapi.NodeGroupRoleFEObserver, 2)
	c := newController(&fakeFE{}, append(configMaps(), cluster(), g, feEndpoints())...)
	_, err := c.Sync(context.Background(), g)
	require.NoError(t, err)

	sts := getSts(t, c, "readers-fe")
	require.True(t, metav1.IsControlledBy(sts, g))
	require.Equal(t, "readers-fe-search", sts.Spec.ServiceName)
	container := sts.Spec.Template.Spec.Containers[0]
	require.Equal(t, "starrocks/fe-ubuntu:4.1.8", container.Image)
	require.Contains(t, container.Env, corev1.EnvVar{Name: srapi.EnvIsFEObserver, Value: "true"})
	// the observers join through the FE service of the cluster, not through their own
	require.Contains(t, container.Env, corev1.EnvVar{Name: "FE_SERVICE_NAME", Value: "sr-fe-service.default"})
	// their pods are not selected by the FE service of the cluster
	require.Equal(t, "readers-fe", sts.Spec.Template.Labels[srapi.OwnerReference])

	for _, name := range []string{"readers-fe-service", "readers-fe-search"} {
		require.NoError(t, c.Client.Get(context.Background(),
			types.NamespacedName{Namespace: namespace, Name: name}, &corev1.Service{}), name)
	}
}

func TestSyncRefusesAStatefulSetItDoesNotOwn(t *testing.T) {
	foreign := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "sr-cn", Namespace: namespace}}
	g := group("sr", srapi.NodeGroupRoleCN, 1)
	c := newController(&fakeFE{}, append(configMaps(), cluster(), g, feEndpoints(), foreign)...)
	_, err := c.Sync(context.Background(), g)
	require.ErrorContains(t, err, "does not belong to this node group")
	require.Equal(t, ReasonSyncFailed, condition(g, srapi.NodeGroupReady).Reason)
	require.Equal(t, srapi.ComponentFailed, g.Status.Phase)
}

// rolledOutSts is the StatefulSet of a group as the StatefulSet controller reports it once every pod is ready.
func rolledOutSts(g *srapi.StarRocksNodeGroup, name string, replicas int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: srapi.GroupVersion.String(), Kind: "StarRocksNodeGroup", Name: g.Name, UID: g.UID,
				Controller: pointer.Bool(true),
			}},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: pointer.Int32(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
		Status: appsv1.StatefulSetStatus{Replicas: replicas, ReadyReplicas: replicas, UpdatedReplicas: replicas},
	}
}

func TestObserveStatus(t *testing.T) {
	host := func(pod string) string { return pod + ".readers-fe-search.default.svc.cluster.local" }
	tests := []struct {
		name           string
		fe             *fakeFE
		wantRegistered int32
		wantReason     string
		wantPhase      srapi.ComponentPhase
		wantDropped    []string
	}{
		{
			name: "all nodes alive",
			fe: &fakeFE{nodes: []Node{
				{Host: host("readers-fe-0"), Alive: true}, {Host: host("readers-fe-1"), Alive: true},
				{Host: "sr-fe-0.sr-fe-search.default.svc.cluster.local", Alive: true},
			}},
			wantRegistered: 2, wantReason: ReasonAllNodesServing, wantPhase: srapi.ComponentRunning,
		},
		{
			name:           "a node is not alive",
			fe:             &fakeFE{nodes: []Node{{Host: host("readers-fe-0"), Alive: true}, {Host: host("readers-fe-1")}}},
			wantRegistered: 1, wantReason: ReasonNodesNotRegistered, wantPhase: srapi.ComponentReconciling,
		},
		{
			name: "a node of a pod that is gone is dropped",
			fe: &fakeFE{nodes: []Node{
				{Host: host("readers-fe-0"), Alive: true}, {Host: host("readers-fe-1"), Alive: true},
				{Host: host("readers-fe-2")},
			}},
			wantRegistered: 2, wantReason: ReasonAllNodesServing, wantPhase: srapi.ComponentRunning,
			wantDropped: []string{host("readers-fe-2")},
		},
		{
			name:       "FE does not answer",
			fe:         &fakeFE{err: errors.New("dial tcp: i/o timeout")},
			wantReason: ReasonFeUnreachable, wantPhase: srapi.ComponentReconciling,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := group("readers", srapi.NodeGroupRoleFEObserver, 2)
			src := cluster()
			c := newController(tt.fe, append(configMaps(), src, g, rolledOutSts(g, "readers-fe", 2))...)
			comp, err := resolve(src, g)
			require.NoError(t, err)
			setCondition(g, srapi.NodeGroupClusterReady, true, ReasonFeReady, "")
			require.NoError(t, c.observe(context.Background(), g, src, comp, map[string]interface{}{}))

			require.Equal(t, tt.wantRegistered, g.Status.RegisteredReplicas)
			require.Equal(t, tt.wantReason, condition(g, srapi.NodeGroupReady).Reason)
			require.Equal(t, tt.wantPhase, g.Status.Phase)
			require.Equal(t, tt.wantDropped, tt.fe.dropped)
			require.Equal(t, int32(2), g.Status.ReadyReplicas)
			require.Equal(t, "app=readers-fe", g.Status.Selector)
		})
	}
}

func TestFinalize(t *testing.T) {
	g := group("spot", srapi.NodeGroupRoleCN, 2)
	feClient := &fakeFE{nodes: []Node{
		{Host: "spot-cn-0.spot-cn-search.default.svc.cluster.local", Port: "9050"},
		{Host: "spot-cn-1.spot-cn-search.default.svc.cluster.local", Port: "9050"},
		{Host: "sr-cn-0.sr-cn-search.default.svc.cluster.local", Port: "9050"},
	}}
	c := newController(feClient, append(configMaps(), cluster(), g, rolledOutSts(g, "spot-cn", 2))...)

	// the StatefulSet goes first, and nothing is dropped while it is there
	require.ErrorIs(t, c.Finalize(context.Background(), g), ErrPodsStillRunning)
	require.Empty(t, feClient.dropped)
	require.Error(t, c.Client.Get(context.Background(),
		types.NamespacedName{Namespace: namespace, Name: "spot-cn"}, &appsv1.StatefulSet{}))

	// then the nodes of the group, and only those
	require.NoError(t, c.Finalize(context.Background(), g))
	require.Equal(t, []string{
		"spot-cn-0.spot-cn-search.default.svc.cluster.local",
		"spot-cn-1.spot-cn-search.default.svc.cluster.local",
	}, feClient.dropped)
}

func TestFinalizeLeavesWhatBelongsToAnotherObject(t *testing.T) {
	// the group "sr" of role cn would run "sr-cn", which is the CN statefulset of the cluster "sr"
	foreign := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "sr-cn", Namespace: namespace}}
	g := group("sr", srapi.NodeGroupRoleCN, 1)
	feClient := &fakeFE{nodes: []Node{{Host: "sr-cn-0.sr-cn-search.default.svc.cluster.local", Port: "9050"}}}
	c := newController(feClient, append(configMaps(), cluster(), g, foreign)...)

	require.NoError(t, c.Finalize(context.Background(), g))
	require.Empty(t, feClient.dropped)
	getSts(t, c, "sr-cn")
}

func TestFinalizeDoesNotWaitForAnFEThatIsGone(t *testing.T) {
	g := group("spot", srapi.NodeGroupRoleCN, 2)
	c := newController(&fakeFE{err: errors.New("connection refused")}, append(configMaps(), cluster(), g)...)

	// a group deleted a moment ago tries again, one deleted long ago gives up
	now := metav1.Now()
	g.DeletionTimestamp = &now
	require.ErrorContains(t, c.Finalize(context.Background(), g), "connection refused")
	longAgo := metav1.NewTime(now.Add(-2 * DropNodesTimeout))
	g.DeletionTimestamp = &longAgo
	require.NoError(t, c.Finalize(context.Background(), g))

	// without its cluster there is no FE to ask
	c = newController(&fakeFE{err: errors.New("must not be called")}, g)
	require.NoError(t, c.Finalize(context.Background(), g))
}
