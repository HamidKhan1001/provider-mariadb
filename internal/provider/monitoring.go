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
	"cmp"
	"fmt"
	"slices"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	"github.com/mariadb-operator/mariadb-operator/v26/pkg/builder"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/definition/components"
	"github.com/openeverest/provider-mariadb/internal/common"
)

// vmIgnorePrometheusUpdatesAnnotation stops the VictoriaMetrics operator's
// Prometheus converter from syncing the operator's ServiceMonitor into the
// VMServiceScrape of the same name, which the provider owns instead.
const vmIgnorePrometheusUpdatesAnnotation = "operator.victoriametrics.com/ignore-prometheus-updates"

// vmServiceScrapeGVK identifies the VictoriaMetrics operator VMServiceScrape.
// It is handled as unstructured to avoid depending on the VictoriaMetrics
// operator API module, which pulls in most of VictoriaMetrics.
var vmServiceScrapeGVK = schema.GroupVersionKind{
	Group:   "operator.victoriametrics.com",
	Version: "v1beta1",
	Kind:    "VMServiceScrape",
}

// monitoringParams decodes the monitoring component parameters from the
// Instance. It returns nil when the monitoring component is absent, making
// monitoring opt-in.
func monitoringParams(c *controller.Context) (*components.MonitoringParameters, error) {
	monitoring, ok := c.Instance().Spec.Components[common.ComponentMonitoring]
	if !ok {
		return nil, nil
	}

	var params components.MonitoringParameters
	if err := c.DecodeComponentParameters(monitoring, &params); err != nil {
		return nil, fmt.Errorf("decode monitoring parameters: %w", err)
	}
	return &params, nil
}

// buildMetrics translates the monitoring component into the mariadb-operator's
// spec.metrics. It returns nil when metrics are not enabled, so the operator
// deploys no exporter and creates no Prometheus objects.
//
// The current mariadb-operator couples the exporter and the ServiceMonitor:
// whenever metrics are enabled it reconciles a ServiceMonitor (and requires the
// ServiceMonitor CRD to be present), so ServiceMonitor.Enabled has no effect yet.
func buildMetrics(c *controller.Context) (*mariadbv1alpha1.MariadbMetrics, error) {
	params, err := monitoringParams(c)
	if err != nil {
		return nil, err
	}
	if params == nil || !params.Enabled {
		return nil, nil
	}

	image, err := resolveExporterImage(c)
	if err != nil {
		return nil, err
	}

	metrics := &mariadbv1alpha1.MariadbMetrics{
		Enabled: true,
		Exporter: mariadbv1alpha1.Exporter{
			Image:       image,
			PodMetadata: &mariadbv1alpha1.Metadata{Labels: c.PodLabels(common.ComponentMonitoring)},
		},
		ServiceMonitor: mariadbv1alpha1.ServiceMonitor{
			PrometheusRelease: params.ServiceMonitor.PrometheusRelease,
			JobLabel:          params.ServiceMonitor.JobLabel,
			Interval:          params.ServiceMonitor.Interval,
			ScrapeTimeout:     params.ServiceMonitor.ScrapeTimeout,
		},
	}

	monitoring := c.Instance().Spec.Components[common.ComponentMonitoring]
	if monitoring.Resources != nil && (monitoring.Resources.Limits != nil || monitoring.Resources.Requests != nil) {
		metrics.Exporter.Resources = &mariadbv1alpha1.ResourceRequirements{
			Limits:   monitoring.Resources.Limits,
			Requests: monitoring.Resources.Requests,
		}
	}

	return metrics, nil
}

// resolveExporterImage returns the mysqld-exporter image from the monitoring
// component override, its selected version, or the provider default — mirroring
// the engine image resolution precedence.
func resolveExporterImage(c *controller.Context) (string, error) {
	monitoring := c.Instance().Spec.Components[common.ComponentMonitoring]
	if monitoring.Image != "" {
		return monitoring.Image, nil
	}

	spec, err := c.ProviderSpec()
	if err != nil {
		return "", fmt.Errorf("get provider spec: %w", err)
	}

	image := ""
	if monitoring.Version != "" {
		image = controller.GetImageForVersion(spec, common.ComponentMonitoring, monitoring.Version)
	}
	if image == "" {
		image = controller.GetDefaultImageForComponent(spec, common.ComponentMonitoring)
	}
	return image, nil
}


// isVictoriaMetricsEnabled reports whether a VMServiceScrape is requested.
func isVictoriaMetricsEnabled(params *components.MonitoringParameters) bool {
	return params != nil && bool(params.Enabled) && bool(params.VictoriaMetrics.Enabled)
}

// vmServiceScrapeName returns the name of the VMServiceScrape for the given
// instance. It matches the operator's ServiceMonitor name, which is also the
// name the VictoriaMetrics Prometheus converter gives its VMServiceScrape.
func vmServiceScrapeName(instanceName string) string {
	mdb := &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: instanceName}}
	return mdb.MetricsKey().Name
}

func newVMServiceScrape() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(vmServiceScrapeGVK)
	return u
}

// SyncVMServiceScrape server-side applies a VMServiceScrape for the exporter
// when VictoriaMetrics is enabled, and deletes it otherwise.
//
// A VMServiceScrape converted from the operator's ServiceMonitor may already
// exist under the same name. It is taken over: the applied spec replaces the
// converted one and the ignore annotation stops further conversions, so the
// exporter is scraped once.
func SyncVMServiceScrape(c *controller.Context) error {
	params, err := monitoringParams(c)
	if err != nil {
		return err
	}
	if !isVictoriaMetricsEnabled(params) {
		return teardownVMServiceScrape(c)
	}

	mdb := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(mdb, c.Name()); err != nil {
		return fmt.Errorf("get MariaDB: %w", err)
	}
	// The cache may not reflect the metrics just applied yet; the MariaDB
	// watch re-enqueues the Instance once it does.
	if !mdb.AreMetricsEnabled() {
		return nil
	}

	desired, err := desiredVMServiceScrape(c, mdb, params.VictoriaMetrics)
	if err != nil {
		return err
	}
	if err := adoptVMServiceScrape(c, desired.GetName()); err != nil {
		return err
	}
	if err := c.Apply(desired); err != nil {
		return fmt.Errorf("apply VMServiceScrape: %w", err)
	}
	return nil
}

// desiredVMServiceScrape translates the ServiceMonitor the operator builds for
// the MariaDB into a VMServiceScrape, so the per-Pod targets and relabelings
// stay identical to the operator's, including the role of each Pod.
func desiredVMServiceScrape(
	c *controller.Context,
	mdb *mariadbv1alpha1.MariaDB,
	params components.VictoriaMetricsParameters,
) (*unstructured.Unstructured, error) {
	svcMonitor, err := builder.NewBuilder(c.Client().Scheme(), nil, nil).BuildServiceMonitor(mdb)
	if err != nil {
		return nil, fmt.Errorf("build ServiceMonitor: %w", err)
	}

	endpoints := make([]any, 0, len(svcMonitor.Spec.Endpoints))
	for _, e := range svcMonitor.Spec.Endpoints {
		endpoint := map[string]any{"port": e.Port}
		setString(endpoint, "path", e.Path)
		setString(endpoint, "interval", cmp.Or(params.Interval, string(e.Interval)))
		setString(endpoint, "scrapeTimeout", cmp.Or(params.ScrapeTimeout, string(e.ScrapeTimeout)))
		if len(e.Params) > 0 {
			query := make(map[string]any, len(e.Params))
			for key, values := range e.Params {
				query[key] = toAnySlice(values)
			}
			endpoint["params"] = query
		}
		if rcs := convertRelabelConfigs(e.RelabelConfigs); len(rcs) > 0 {
			endpoint["relabelConfigs"] = rcs
		}
		if rcs := convertRelabelConfigs(e.MetricRelabelConfigs); len(rcs) > 0 {
			endpoint["metricRelabelConfigs"] = rcs
		}
		endpoints = append(endpoints, endpoint)
	}

	matchLabels := make(map[string]any, len(svcMonitor.Spec.Selector.MatchLabels))
	for k, v := range svcMonitor.Spec.Selector.MatchLabels {
		matchLabels[k] = v
	}

	objMeta := c.ObjectMeta(svcMonitor.Name)
	vmss := newVMServiceScrape()
	vmss.SetName(objMeta.Name)
	vmss.SetNamespace(objMeta.Namespace)
	vmss.SetLabels(objMeta.Labels)
	vmss.SetAnnotations(map[string]string{vmIgnorePrometheusUpdatesAnnotation: "enabled"})
	vmss.Object["spec"] = map[string]any{
		"selector":          map[string]any{"matchLabels": matchLabels},
		"namespaceSelector": map[string]any{"matchNames": toAnySlice(svcMonitor.Spec.NamespaceSelector.MatchNames)},
		"endpoints":         endpoints,
	}
	return vmss, nil
}

// convertRelabelConfigs maps Prometheus relabel configs onto the
// VMServiceScrape ones, which use the same field names.
func convertRelabelConfigs(in []monitoringv1.RelabelConfig) []any {
	out := make([]any, 0, len(in))
	for _, rc := range in {
		config := map[string]any{}
		if len(rc.SourceLabels) > 0 {
			labels := make([]any, 0, len(rc.SourceLabels))
			for _, label := range rc.SourceLabels {
				labels = append(labels, string(label))
			}
			config["sourceLabels"] = labels
		}
		setString(config, "separator", ptr.Deref(rc.Separator, ""))
		setString(config, "targetLabel", rc.TargetLabel)
		setString(config, "regex", rc.Regex)
		setString(config, "replacement", ptr.Deref(rc.Replacement, ""))
		setString(config, "action", rc.Action)
		if rc.Modulus != 0 {
			config["modulus"] = int64(rc.Modulus)
		}
		out = append(out, config)
	}
	return out
}

func setString(m map[string]any, key, value string) {
	if value != "" {
		m[key] = value
	}
}

func toAnySlice(values []string) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, v)
	}
	return out
}

// adoptVMServiceScrape drops the controller reference from a VMServiceScrape
// the provider does not control yet. The Prometheus converter sets one to the
// ServiceMonitor when its owner references are enabled, and an object can
// only have a single controller.
func adoptVMServiceScrape(c *controller.Context, name string) error {
	existing := newVMServiceScrape()
	if err := c.Get(existing, name); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get VMServiceScrape: %w", err)
	}
	if metav1.IsControlledBy(existing, c.Instance()) || metav1.GetControllerOf(existing) == nil {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.SetOwnerReferences(slices.DeleteFunc(existing.GetOwnerReferences(), func(ref metav1.OwnerReference) bool {
		return ptr.Deref(ref.Controller, false)
	}))
	if err := c.Client().Patch(c.Context(), existing, patch); err != nil {
		return fmt.Errorf("adopt VMServiceScrape %q: %w", name, err)
	}
	return nil
}

// teardownVMServiceScrape deletes the VMServiceScrape the provider created.
// Objects it does not control, such as the ones the Prometheus converter
// creates, are left alone. A missing CRD means there is nothing to delete.
func teardownVMServiceScrape(c *controller.Context) error {
	vmss := newVMServiceScrape()
	if err := c.Get(vmss, vmServiceScrapeName(c.Name())); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("get VMServiceScrape for teardown: %w", err)
	}
	if !metav1.IsControlledBy(vmss, c.Instance()) || vmss.GetDeletionTimestamp() != nil {
		return nil
	}
	if err := c.Delete(vmss); err != nil {
		return fmt.Errorf("delete VMServiceScrape %q: %w", vmss.GetName(), err)
	}
	return nil
}

// vmServiceScrapeCRDInstalled reports whether the VMServiceScrape CRD is
// served by the cluster.
func vmServiceScrapeCRDInstalled(c *controller.Context) (bool, error) {
	_, err := c.Client().RESTMapper().RESTMapping(vmServiceScrapeGVK.GroupKind(), vmServiceScrapeGVK.Version)
	if meta.IsNoMatchError(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up VMServiceScrape CRD: %w", err)
	}
	return true, nil
}
