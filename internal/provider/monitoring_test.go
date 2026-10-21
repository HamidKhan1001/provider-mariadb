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
	"fmt"
	"strings"
	"testing"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/definition/components"
	"github.com/openeverest/provider-mariadb/internal/common"
)

// newMonitoringContext builds a controller.Context for an Instance whose
// monitoring component carries the given parameters and image override. A nil
// params map means the monitoring component is omitted entirely.
func newMonitoringContext(t *testing.T, params *components.MonitoringParameters, image string) *controller.Context {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
		Spec: corev1alpha1.InstanceSpec{
			Components: map[string]corev1alpha1.ComponentSpec{},
		},
	}

	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		instance.Spec.Components[common.ComponentMonitoring] = corev1alpha1.ComponentSpec{
			Type:       common.ComponentMonitoring,
			Image:      image,
			Parameters: &runtime.RawExtension{Raw: raw},
		}
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(instance).Build()
	return controller.NewContext(context.Background(), c, instance, common.ProviderName)
}

func TestBuildMetrics_MonitoringAbsent(t *testing.T) {
	ctx := newMonitoringContext(t, nil, "")

	metrics, err := buildMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics != nil {
		t.Fatalf("expected nil metrics when monitoring absent, got %+v", metrics)
	}
}

func TestBuildMetrics_Disabled(t *testing.T) {
	ctx := newMonitoringContext(t, &components.MonitoringParameters{Enabled: false}, "exporter:latest")

	metrics, err := buildMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics != nil {
		t.Fatalf("expected nil metrics when disabled, got %+v", metrics)
	}
}

func TestBuildMetrics_EnabledWithImageOverride(t *testing.T) {
	params := &components.MonitoringParameters{
		Enabled: true,
		ServiceMonitor: components.ServiceMonitorParameters{
			PrometheusRelease: "kube-prometheus-stack",
			JobLabel:          "app.kubernetes.io/instance",
			Interval:          "30s",
			ScrapeTimeout:     "10s",
		},
	}
	ctx := newMonitoringContext(t, params, "custom/mysqld-exporter:v1")

	metrics, err := buildMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics == nil {
		t.Fatal("expected metrics, got nil")
	}
	if !metrics.Enabled {
		t.Error("expected metrics.Enabled to be true")
	}
	if metrics.Exporter.Image != "custom/mysqld-exporter:v1" {
		t.Errorf("expected exporter image override, got %q", metrics.Exporter.Image)
	}
	if metrics.ServiceMonitor.PrometheusRelease != "kube-prometheus-stack" {
		t.Errorf("unexpected prometheusRelease: %q", metrics.ServiceMonitor.PrometheusRelease)
	}
	if metrics.ServiceMonitor.JobLabel != "app.kubernetes.io/instance" {
		t.Errorf("unexpected jobLabel: %q", metrics.ServiceMonitor.JobLabel)
	}
	if metrics.ServiceMonitor.Interval != "30s" {
		t.Errorf("unexpected interval: %q", metrics.ServiceMonitor.Interval)
	}
	if metrics.ServiceMonitor.ScrapeTimeout != "10s" {
		t.Errorf("unexpected scrapeTimeout: %q", metrics.ServiceMonitor.ScrapeTimeout)
	}
}

func TestBuildMetrics_ExporterResources(t *testing.T) {
	params := &components.MonitoringParameters{Enabled: true}
	ctx := newMonitoringContext(t, params, "exporter:latest")

	// Attach resources to the monitoring component.
	monitoring := ctx.Instance().Spec.Components[common.ComponentMonitoring]
	monitoring.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("100m"),
		},
	}
	ctx.Instance().Spec.Components[common.ComponentMonitoring] = monitoring

	metrics, err := buildMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics == nil || metrics.Exporter.Resources == nil {
		t.Fatal("expected exporter resources to be set")
	}
	if got := metrics.Exporter.Resources.Requests.Cpu().String(); got != "100m" {
		t.Errorf("unexpected cpu request: %q", got)
	}
}

// The OpenEverest UI writes select values as strings, so `enabled` can arrive as
// the quoted string "true" rather than a JSON bool. FlexBool must decode it.
func TestBuildMetrics_EnabledAsString(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: corev1alpha1.InstanceSpec{
			Components: map[string]corev1alpha1.ComponentSpec{
				common.ComponentMonitoring: {
					Type:       common.ComponentMonitoring,
					Image:      "exporter:latest",
					Parameters: &runtime.RawExtension{Raw: []byte(`{"enabled":"true"}`)},
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(instance).Build()
	ctx := controller.NewContext(context.Background(), c, instance, common.ProviderName)

	metrics, err := buildMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error decoding string-valued enabled: %v", err)
	}
	if metrics == nil || !metrics.Enabled {
		t.Fatal("expected metrics enabled when enabled is the string \"true\"")
	}
}


// newVictoriaMetricsContext builds a controller.Context for an Instance with
// the given monitoring parameters and a fake client whose REST mapper serves
// the VMServiceScrape CRD when crdInstalled is set.
func newVictoriaMetricsContext(t *testing.T, params string, crdInstalled bool, objs ...client.Object) *controller.Context {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, mariadbv1alpha1.AddToScheme(scheme))

	mapper := meta.NewDefaultRESTMapper(nil)
	for gvk := range scheme.AllKnownTypes() {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	if crdInstalled {
		mapper.Add(vmServiceScrapeGVK, meta.RESTScopeNamespace)
	}

	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid"},
		Spec: corev1alpha1.InstanceSpec{
			Components: map[string]corev1alpha1.ComponentSpec{
				common.ComponentMonitoring: {
					Type:       common.ComponentMonitoring,
					Image:      "exporter:test",
					Parameters: &runtime.RawExtension{Raw: []byte(params)},
				},
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(mapper).
		WithObjects(append([]client.Object{instance}, objs...)...).
		Build()
	return controller.NewContext(context.Background(), cl, instance, common.ProviderName)
}

// metricsMariaDB returns a 3-replica MariaDB with metrics enabled whose
// primary is the Pod with the given index.
func metricsMariaDB(primary int) *mariadbv1alpha1.MariaDB {
	return &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: mariadbv1alpha1.MariaDBSpec{
			Replicas: 3,
			Port:     3306,
			Metrics: &mariadbv1alpha1.MariadbMetrics{
				Enabled:        true,
				ServiceMonitor: mariadbv1alpha1.ServiceMonitor{Interval: "10s"},
			},
		},
		Status: mariadbv1alpha1.MariaDBStatus{CurrentPrimaryPodIndex: ptr.To(primary)},
	}
}

func getVMServiceScrape(t *testing.T, c *controller.Context) *unstructured.Unstructured {
	t.Helper()
	vmss := newVMServiceScrape()
	require.NoError(t, c.Get(vmss, "test-metrics"))
	return vmss
}

func TestDesiredVMServiceScrape(t *testing.T) {
	c := newVictoriaMetricsContext(t, `{"enabled":true,"victoriaMetrics":{"enabled":true}}`, true)
	vmss, err := desiredVMServiceScrape(c, metricsMariaDB(1), components.VictoriaMetricsParameters{Interval: "30s"})
	require.NoError(t, err)

	assert.Equal(t, "test-metrics", vmss.GetName())
	assert.Equal(t, "enabled", vmss.GetAnnotations()[vmIgnorePrometheusUpdatesAnnotation])
	assert.Equal(t, "everest", vmss.GetLabels()["app.kubernetes.io/managed-by"])

	matchLabels, _, _ := unstructured.NestedStringMap(vmss.Object, "spec", "selector", "matchLabels")
	assert.Equal(t, map[string]string{
		"app.kubernetes.io/name":     "exporter",
		"app.kubernetes.io/instance": "test-metrics",
	}, matchLabels)
	namespaces, _, _ := unstructured.NestedStringSlice(vmss.Object, "spec", "namespaceSelector", "matchNames")
	assert.Equal(t, []string{"default"}, namespaces)

	endpoints, _, _ := unstructured.NestedSlice(vmss.Object, "spec", "endpoints")
	require.Len(t, endpoints, 3)
	for i, raw := range endpoints {
		endpoint := raw.(map[string]any)
		assert.Equal(t, "metrics", endpoint["port"])
		assert.Equal(t, "/probe", endpoint["path"])
		assert.Equal(t, "30s", endpoint["interval"], "the VictoriaMetrics interval overrides the ServiceMonitor one")
		assert.NotContains(t, endpoint, "scrapeTimeout")
		target := fmt.Sprintf("test-%d.test-internal.default.svc.cluster.local:3306", i)
		assert.Equal(t, map[string]any{"target": []any{target}}, endpoint["params"])

		role := "replica"
		if i == 1 {
			role = "primary"
		}
		assert.Equal(t, []any{map[string]any{
			"action": "replace", "replacement": role, "targetLabel": "role",
		}}, endpoint["relabelConfigs"])
		metricRelabels := endpoint["metricRelabelConfigs"].([]any)
		require.Len(t, metricRelabels, 2)
		assert.Equal(t, map[string]any{
			"action": "replace", "replacement": strings.Split(target, ":")[0],
			"sourceLabels": []any{"instance"}, "targetLabel": "instance",
		}, metricRelabels[0])
	}
}

func TestSyncVMServiceScrape_CreatesAndTearsDown(t *testing.T) {
	c := newVictoriaMetricsContext(t, `{"enabled":true,"victoriaMetrics":{"enabled":"true"}}`, true, metricsMariaDB(0))
	require.NoError(t, SyncVMServiceScrape(c))

	vmss := getVMServiceScrape(t, c)
	assert.True(t, metav1.IsControlledBy(vmss, c.Instance()))
	endpoints, _, _ := unstructured.NestedSlice(vmss.Object, "spec", "endpoints")
	assert.Len(t, endpoints, 3)
	assert.Equal(t, "10s", endpoints[0].(map[string]any)["interval"], "defaults to the ServiceMonitor interval")

	c.Instance().Spec.Components[common.ComponentMonitoring] = corev1alpha1.ComponentSpec{
		Type:       common.ComponentMonitoring,
		Parameters: &runtime.RawExtension{Raw: []byte(`{"enabled":true}`)},
	}
	require.NoError(t, SyncVMServiceScrape(c))
	err := c.Get(newVMServiceScrape(), "test-metrics")
	assert.True(t, apierrors.IsNotFound(err), "expected VMServiceScrape to be deleted, got %v", err)
}

// A VMServiceScrape converted from the operator's ServiceMonitor is taken over,
// including when the converter made the ServiceMonitor its controller.
func TestSyncVMServiceScrape_AdoptsConverted(t *testing.T) {
	converted := newVMServiceScrape()
	converted.SetName("test-metrics")
	converted.SetNamespace("default")
	converted.SetLabels(map[string]string{"release": "kube-prometheus-stack"})
	converted.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "monitoring.coreos.com/v1", Kind: "ServiceMonitor", Name: "test-metrics",
		UID: "sm-uid", Controller: ptr.To(true),
	}})
	converted.Object["spec"] = map[string]any{"endpoints": []any{map[string]any{"port": "metrics"}}}

	c := newVictoriaMetricsContext(t, `{"enabled":true,"victoriaMetrics":{"enabled":true}}`, true, metricsMariaDB(0), converted)
	require.NoError(t, SyncVMServiceScrape(c))

	vmss := getVMServiceScrape(t, c)
	assert.True(t, metav1.IsControlledBy(vmss, c.Instance()))
	require.Len(t, vmss.GetOwnerReferences(), 1, "the converter's controller reference is dropped")
	assert.Equal(t, "enabled", vmss.GetAnnotations()[vmIgnorePrometheusUpdatesAnnotation])
	endpoints, _, _ := unstructured.NestedSlice(vmss.Object, "spec", "endpoints")
	assert.Len(t, endpoints, 3)
}

func TestSyncVMServiceScrape_TeardownLeavesForeign(t *testing.T) {
	converted := newVMServiceScrape()
	converted.SetName("test-metrics")
	converted.SetNamespace("default")

	c := newVictoriaMetricsContext(t, `{"enabled":true}`, true, metricsMariaDB(0), converted)
	require.NoError(t, SyncVMServiceScrape(c))
	require.NoError(t, c.Get(newVMServiceScrape(), "test-metrics"), "a VMServiceScrape the provider does not control is kept")

	// Without the CRD there is nothing to tear down.
	c = newVictoriaMetricsContext(t, `{"enabled":true}`, false)
	require.NoError(t, SyncVMServiceScrape(c))
}

func TestValidateMonitoring_VictoriaMetrics(t *testing.T) {
	tests := []struct {
		name         string
		params       string
		crdInstalled bool
		wantErr      string
	}{
		{name: "metrics only", params: `{"enabled":true}`},
		{name: "enabled with CRD", params: `{"enabled":true,"victoriaMetrics":{"enabled":true}}`, crdInstalled: true},
		{
			name:    "without metrics",
			params:  `{"victoriaMetrics":{"enabled":true}}`,
			wantErr: "requires metrics to be enabled",
		},
		{
			name:    "without CRD",
			params:  `{"enabled":true,"victoriaMetrics":{"enabled":true}}`,
			wantErr: "requires the VMServiceScrape CRD",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMonitoring(newVictoriaMetricsContext(t, tt.params, tt.crdInstalled))
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}
