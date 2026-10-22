// Copyright (C) 2026 The OpenEverest Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package provider

import (
	"context"
	"encoding/json"
	"testing"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/internal/common"
)

// syncHarness is a context for an Instance backed by a fake client that records
// the body of every server-side apply of the MariaDB. The fake client's merge of
// CRDs round-trips through the typed struct, so assertions target the applied
// body; merge semantics are covered by the integration suites.
type syncHarness struct {
	*controller.Context
	applied []map[string]any
}

func newSyncHarness(t *testing.T, topology string, components map[string]corev1alpha1.ComponentSpec, objs ...client.Object) *syncHarness {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, mariadbv1alpha1.AddToScheme(scheme))

	all := map[string]corev1alpha1.ComponentSpec{
		common.ComponentEngine: {Type: common.ComponentTypeMariaDB, Image: "mariadb:12.3"},
	}
	for name, comp := range components {
		all[name] = comp
	}
	in := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid"},
		Spec:       corev1alpha1.InstanceSpec{Components: all},
	}
	if topology != "" {
		in.Spec.Topology = &corev1alpha1.TopologySpec{Type: topology}
	}

	h := &syncHarness{}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithReturnManagedFields().
		WithObjects(append([]client.Object{in}, objs...)...).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				data, err := json.Marshal(obj)
				require.NoError(t, err)
				var body map[string]any
				require.NoError(t, json.Unmarshal(data, &body))
				h.applied = append(h.applied, body)
				return cl.Apply(ctx, obj, opts...)
			},
		}).
		Build()
	h.Context = controller.NewContext(context.Background(), cl, in, common.ProviderName)
	return h
}

// lastSpec returns the spec of the most recent apply.
func (h *syncHarness) lastSpec(t *testing.T) map[string]any {
	t.Helper()
	require.NotEmpty(t, h.applied)
	spec, ok := h.applied[len(h.applied)-1]["spec"].(map[string]any)
	require.True(t, ok)
	return spec
}

func monitoringComponent(enabled bool) map[string]corev1alpha1.ComponentSpec {
	raw := `{"enabled":false}`
	if enabled {
		raw = `{"enabled":true}`
	}
	return map[string]corev1alpha1.ComponentSpec{
		common.ComponentMonitoring: {
			Type:       common.ComponentMonitoring,
			Image:      "prom/mysqld-exporter:test",
			Parameters: &runtime.RawExtension{Raw: []byte(raw)},
		},
	}
}

func TestSyncMariaDB_Repeatable(t *testing.T) {
	h := newSyncHarness(t, "replication", nil)
	require.NoError(t, SyncMariaDB(h.Context))

	engine := h.Instance().Spec.Components[common.ComponentEngine]
	engine.Replicas = ptr.To(int32(5))
	h.Instance().Spec.Components[common.ComponentEngine] = engine
	require.NoError(t, SyncMariaDB(h.Context), "a repeated sync must not fail")

	assert.EqualValues(t, 5, h.lastSpec(t)["replicas"])
	mdb := &mariadbv1alpha1.MariaDB{}
	require.NoError(t, h.Get(mdb, h.Name()))
	assert.Equal(t, int32(5), mdb.Spec.Replicas)
}

func TestSyncMariaDB_StopsDeclaringMetricsWhenMonitoringDisabled(t *testing.T) {
	h := newSyncHarness(t, "", monitoringComponent(true))
	require.NoError(t, SyncMariaDB(h.Context))
	require.Contains(t, h.lastSpec(t), "metrics")

	h.Instance().Spec.Components[common.ComponentMonitoring] = monitoringComponent(false)[common.ComponentMonitoring]
	require.NoError(t, SyncMariaDB(h.Context))
	assert.NotContains(t, h.lastSpec(t), "metrics", "omitting spec.metrics makes the apply remove it")
}

func TestDesiredMariaDB_SchedulingPolicy(t *testing.T) {
	h := newSyncHarness(t, "galera", nil)
	engine := h.Instance().Spec.Components[common.ComponentEngine]
	engine.SchedulingPolicy = &commonv1alpha1.SchedulingPolicy{
		NodeSelector: map[string]string{"disktype": "ssd"},
		Tolerations:  []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}},
		TopologySpreadConstraints: &[]corev1.TopologySpreadConstraint{
			{MaxSkew: 1, TopologyKey: corev1.LabelTopologyZone, WhenUnsatisfiable: corev1.ScheduleAnyway},
			{
				MaxSkew: 1, TopologyKey: corev1.LabelHostname, WhenUnsatisfiable: corev1.DoNotSchedule,
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "custom"}},
			},
		},
	}
	h.Instance().Spec.Components[common.ComponentEngine] = engine

	mdb, err := desiredMariaDB(h.Context)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"disktype": "ssd"}, mdb.Spec.NodeSelector)
	assert.Equal(t, []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}}, mdb.Spec.Tolerations)
	require.Len(t, mdb.Spec.TopologySpreadConstraints, 2)
	assert.Equal(t, mariadbPodLabels("test"), mdb.Spec.TopologySpreadConstraints[0].LabelSelector.MatchLabels,
		"a constraint without a selector counts the MariaDB pods")
	assert.Equal(t, map[string]string{"app": "custom"}, mdb.Spec.TopologySpreadConstraints[1].LabelSelector.MatchLabels)
	assert.Equal(t, defaultHAAffinity("test"), mdb.Spec.Affinity, "an unset affinity keeps the HA default")
}

func TestDesiredMariaDB_NoTopologySpreadByDefault(t *testing.T) {
	h := newSyncHarness(t, "galera", nil)
	mdb, err := desiredMariaDB(h.Context)
	require.NoError(t, err)
	assert.Nil(t, mdb.Spec.TopologySpreadConstraints)
}

func TestSyncMariaDB_LabelsComponentPods(t *testing.T) {
	h := newSyncHarness(t, "", monitoringComponent(true))
	require.NoError(t, SyncMariaDB(h.Context))

	spec := h.lastSpec(t)
	podLabels := func(component string) map[string]any {
		return map[string]any{"labels": map[string]any{
			"core.openeverest.io/component": component,
			"core.openeverest.io/instance":  "test",
			"core.openeverest.io/provider":  common.ProviderName,
		}}
	}
	assert.Equal(t, podLabels(common.ComponentEngine), spec["podMetadata"])
	exporter := spec["metrics"].(map[string]any)["exporter"].(map[string]any)
	assert.Equal(t, podLabels(common.ComponentMonitoring), exporter["podMetadata"])
	assert.Equal(t, []string{common.ComponentEngine, common.ComponentMonitoring}, h.LabelledComponents())
}

// TestSyncMariaDB_InheritsInstanceLabelOntoChildResources covers openeverest/provider-mariadb#92:
// plugin-metrics finds a Backup's ServiceMonitor by the instance label, but the
// operator only copies InheritMetadata (not PodMetadata) onto resources like
// the ServiceMonitor, so the label has to be set there specifically.
func TestSyncMariaDB_InheritsInstanceLabelOntoChildResources(t *testing.T) {
	h := newSyncHarness(t, "", nil)
	require.NoError(t, SyncMariaDB(h.Context))

	spec := h.lastSpec(t)
	assert.Equal(t, map[string]any{"labels": map[string]any{
		"core.openeverest.io/instance": "test",
	}}, spec["inheritMetadata"])
}

func TestSyncMariaDB_KeepsDeclaringImmutableCreationFields(t *testing.T) {
	existing := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: mariadbv1alpha1.MariaDBSpec{
			Storage: mariadbv1alpha1.Storage{StorageClassName: "fast"},
			BootstrapFrom: &mariadbv1alpha1.BootstrapFrom{
				BackupContentType: mariadbv1alpha1.BackupContentTypePhysical,
				S3:                &mariadbv1alpha1.S3{Bucket: "b", Endpoint: "e"},
			},
		},
	}
	h := newSyncHarness(t, "", nil, existing)
	require.NoError(t, SyncMariaDB(h.Context))

	spec := h.lastSpec(t)
	assert.Equal(t, map[string]any{
		"backupContentType": "Physical",
		"s3":                map[string]any{"bucket": "b", "endpoint": "e"},
	}, spec["bootstrapFrom"])
	assert.Equal(t, "fast", spec["storage"].(map[string]any)["storageClassName"])
}

func TestSyncMariaDB_MigratesLegacyFieldOwnership(t *testing.T) {
	h := newSyncHarness(t, "", nil)
	legacy := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec:       mariadbv1alpha1.MariaDBSpec{Image: "mariadb:11.8", Replicas: 1},
	}
	require.NoError(t, h.Client().Create(h.Context.Context(), legacy, client.FieldOwner(legacyFieldManager)))
	require.NoError(t, h.Get(legacy, h.Name()))
	require.True(t, hasLegacyUpdateManager(legacy.ManagedFields))

	require.NoError(t, SyncMariaDB(h.Context))

	mdb := &mariadbv1alpha1.MariaDB{}
	require.NoError(t, h.Get(mdb, h.Name()))
	assert.False(t, hasLegacyUpdateManager(mdb.ManagedFields), "legacy Update ownership must be handed over")
	var owners []string
	for _, e := range mdb.ManagedFields {
		owners = append(owners, e.Manager+"/"+string(e.Operation))
	}
	assert.Contains(t, owners, applyFieldManager+"/Apply")
	assert.Equal(t, "mariadb:12.3", mdb.Spec.Image)
}

// The applied object must only carry fields the provider set: zero values of
// the operator's non-omitempty fields would take ownership of, and clobber,
// operator defaults.
func TestToApplyObject_OnlyDeclaresProviderFields(t *testing.T) {
	for _, topology := range []string{"", "galera", "replication"} {
		t.Run("topology="+topology, func(t *testing.T) {
			h := newSyncHarness(t, topology, monitoringComponent(true))
			desired, err := desiredMariaDB(h.Context)
			require.NoError(t, err)
			obj, err := toApplyObject(h.Context, desired)
			require.NoError(t, err)

			assertNoEmptyStrings(t, obj.Object, "")
			spec := obj.Object["spec"].(map[string]any)
			switch topology {
			case "galera":
				assert.Equal(t, map[string]any{"enabled": true}, spec["galera"])
			case "replication":
				assert.Equal(t, map[string]any{"enabled": true, "semiSyncBootAsReplica": true}, spec["replication"])
			}
			metrics := spec["metrics"].(map[string]any)
			assert.NotContains(t, metrics, "passwordSecretKeyRef")
			assert.Equal(t, "MariaDB", obj.GetKind())
		})
	}
}

func assertNoEmptyStrings(t *testing.T, v any, path string) {
	t.Helper()
	switch val := v.(type) {
	case string:
		assert.NotEmpty(t, val, "empty string at %s", path)
	case map[string]any:
		for k, child := range val {
			assertNoEmptyStrings(t, child, path+"."+k)
		}
	case []any:
		for _, child := range val {
			assertNoEmptyStrings(t, child, path+"[]")
		}
	}
}

func TestPruneUnset(t *testing.T) {
	got := map[string]any{
		"keepFalse": false,
		"keepZero":  int64(0),
		"dropNil":   nil,
		"dropEmpty": "",
		"nested": map[string]any{
			"initContainer": map[string]any{"image": ""},
			"enabled":       true,
		},
		"dropEmptyList": []any{},
		"list":          []any{map[string]any{"drop": "", "keep": "x"}},
	}
	pruneUnset(got)
	assert.Equal(t, map[string]any{
		"keepFalse": false,
		"keepZero":  int64(0),
		"nested":    map[string]any{"enabled": true},
		"list":      []any{map[string]any{"keep": "x"}},
	}, got)
}

func TestHasLegacyUpdateManager(t *testing.T) {
	assert.False(t, hasLegacyUpdateManager(nil))
	assert.False(t, hasLegacyUpdateManager([]metav1.ManagedFieldsEntry{
		{Manager: applyFieldManager, Operation: metav1.ManagedFieldsOperationApply},
		{Manager: "mariadb-operator", Operation: metav1.ManagedFieldsOperationUpdate},
		{Manager: legacyFieldManager, Operation: metav1.ManagedFieldsOperationUpdate, Subresource: "status"},
	}))
	assert.True(t, hasLegacyUpdateManager([]metav1.ManagedFieldsEntry{
		{Manager: legacyFieldManager, Operation: metav1.ManagedFieldsOperationUpdate},
	}))
}
