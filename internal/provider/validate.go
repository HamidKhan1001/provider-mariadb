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
	"fmt"
	"regexp"
	"strconv"
	"strings"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/definition"
	"github.com/openeverest/provider-mariadb/internal/common"
)

// ValidateMariaDB validates the Instance spec for the MariaDB provider.
func ValidateMariaDB(c *controller.Context) error {
	l := log.FromContext(c.Context())
	l.Info("Validating MariaDB instance", "name", c.Name())

	if err := validateComponents(c); err != nil {
		l.Error(err, "Component validation failed", "name", c.Name())
		return fmt.Errorf("component validation: %w", err)
	}

	if err := validateMonitoring(c); err != nil {
		l.Error(err, "Monitoring validation failed", "name", c.Name())
		return fmt.Errorf("monitoring validation: %w", err)
	}

	if err := validateProxy(c); err != nil {
		l.Error(err, "Proxy validation failed", "name", c.Name())
		return fmt.Errorf("proxy validation: %w", err)
	}

	if err := validateTopology(c); err != nil {
		l.Error(err, "Topology validation failed", "name", c.Name())
		return fmt.Errorf("topology validation: %w", err)
	}

	if err := validateEngineUpdate(c); err != nil {
		l.Error(err, "Engine update validation failed", "name", c.Name())
		return fmt.Errorf("engine update validation: %w", err)
	}

	if err := validateTLS(c); err != nil {
		l.Error(err, "TLS validation failed", "name", c.Name())
		return fmt.Errorf("TLS validation: %w", err)
	}

	if err := validatePITR(c); err != nil {
		l.Error(err, "PITR validation failed", "name", c.Name())
		return fmt.Errorf("PITR validation: %w", err)
	}

	if err := validatePointInTimeDataSource(c); err != nil {
		l.Error(err, "Data source validation failed", "name", c.Name())
		return fmt.Errorf("data source validation: %w", err)
	}

	return nil
}

// validatePointInTimeDataSource rejects point-in-time recovery into a
// non-replication Instance: mariadb-operator only replays binary logs on
// replication clusters, so such a recovery would silently stop at the base
// backup. Only checked at creation, so already-recovered Instances keep
// validating.
func validatePointInTimeDataSource(c *controller.Context) error {
	ds := c.Instance().Spec.DataSource
	if ds == nil || ds.Type != backupv1alpha1.DataSourceTypePointInTime {
		return nil
	}
	if normalizeTopology(c.Instance().GetTopologyType()) == definition.TopologyTypeReplication {
		return nil
	}
	if err := c.Get(&mariadbv1alpha1.MariaDB{}, c.Name()); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get MariaDB: %w", err)
	}
	return fmt.Errorf(
		"point-in-time recovery is only supported into the %q topology; set spec.topology.type to %q",
		definition.TopologyTypeReplication,
		definition.TopologyTypeReplication,
	)
}

// validatePITR enforces the constraints of point-in-time recovery: the
// mariadb-operator archives binary logs from the single primary of the
// asynchronous replication topology, so PITR requires that topology, at most one
// PITR-enabled storage, and a physical backup schedule on that storage to serve
// as the full base backup.
func validatePITR(c *controller.Context) error {
	storage, err := pitrEnabledStorage(c.Instance().Spec.Backup)
	if err != nil {
		return err
	}
	if storage == nil {
		return nil
	}
	if normalizeTopology(c.Instance().GetTopologyType()) != definition.TopologyTypeReplication {
		return fmt.Errorf(
			"point-in-time recovery is only supported on the %q topology; storage %q has pitr.enabled=true",
			definition.TopologyTypeReplication,
			storage.StorageRef.Name,
		)
	}
	if _, err := firstPhysicalSchedule(storage); err != nil {
		return err
	}
	return nil
}

// validateMonitoring ensures the monitoring component parameters are decodable
// and, when metrics are enabled, that an exporter image can be resolved.
func validateMonitoring(c *controller.Context) error {
	params, err := monitoringParams(c)
	if err != nil {
		return err
	}
	if params == nil || !params.Enabled {
		return nil
	}

	image, err := resolveExporterImage(c)
	if err != nil {
		return err
	}
	if image == "" {
		return fmt.Errorf("unable to resolve a mysqld-exporter image for the monitoring component")
	}
	return nil
}

// validateProxy checks the proxy component when MaxScale is enabled: it needs
// an HA topology to front, at least one replica, a supported Service type and
// a resolvable image.
func validateProxy(c *controller.Context) error {
	enabled, err := isProxyEnabled(c)
	if err != nil || !enabled {
		return err
	}
	if !isHATopology(c) {
		return fmt.Errorf(
			"the %q component is only supported on the %q and %q topologies",
			common.ComponentProxy,
			definition.TopologyTypeGalera,
			definition.TopologyTypeReplication,
		)
	}

	proxy := c.Instance().Spec.Components[common.ComponentProxy]
	if proxy.Replicas != nil && *proxy.Replicas < 1 {
		return fmt.Errorf("proxy replicas must be at least 1, got %d", *proxy.Replicas)
	}
	if err := validateService(common.ComponentProxy, proxy.Service); err != nil {
		return err
	}
	if proxy.SchedulingPolicy != nil {
		if err := validateAffinity(proxy.SchedulingPolicy.Affinity); err != nil {
			return err
		}
	}

	image, err := resolveMaxScaleImage(c)
	if err != nil {
		return err
	}
	if image == "" {
		return fmt.Errorf("unable to resolve a MaxScale image for the %q component", common.ComponentProxy)
	}
	return nil
}

// validateComponents verifies that required components are present and their values are sane.
func validateComponents(c *controller.Context) error {
	engine, ok := c.Instance().Spec.Components[common.ComponentEngine]
	if !ok {
		return fmt.Errorf("component %q is required", common.ComponentEngine)
	}

	if engine.Replicas != nil && *engine.Replicas < 1 {
		return fmt.Errorf("engine replicas must be at least 1, got %d", *engine.Replicas)
	}

	if engine.Storage != nil && !engine.Storage.Size.IsZero() {
		if engine.Storage.Size.Cmp(mustParseQuantity("1Gi")) < 0 {
			return fmt.Errorf("engine storage size must be at least 1Gi")
		}
	}

	if err := validateService(common.ComponentEngine, engine.Service); err != nil {
		return err
	}

	if schedulingPolicy := engine.SchedulingPolicy; schedulingPolicy != nil {
		if err := validateAffinity(schedulingPolicy.Affinity); err != nil {
			return err
		}
	}

	return nil
}

// validateService ensures the requested Service type is one the operator supports.
func validateService(componentName string, svc *corev1alpha1.Service) error {
	if svc == nil || svc.ServiceType == "" {
		return nil
	}

	switch svc.ServiceType {
	case corev1.ServiceTypeClusterIP,
		corev1.ServiceTypeLoadBalancer,
		corev1.ServiceTypeNodePort:
		return nil
	default:
		return fmt.Errorf(
			"spec.components.%s.service.serviceType must be one of ClusterIP, LoadBalancer or NodePort",
			componentName,
		)
	}
}

// validateTopology checks the selected topology, its per-topology replica
// rules, and that the topology is not being changed on an existing instance.
func validateTopology(c *controller.Context) error {
	topology := c.Instance().GetTopologyType()
	switch topology {
	case "", string(definition.TopologyTypeStandalone):
		if err := validateStandaloneReplicas(c); err != nil {
			return err
		}
	case string(definition.TopologyTypeGalera):
		if err := validateGaleraReplicas(c); err != nil {
			return err
		}
	case string(definition.TopologyTypeReplication):
		if err := validateReplicationReplicas(c); err != nil {
			return err
		}
		if err := validateReplicationScaleOut(c); err != nil {
			return err
		}
	default:
		return fmt.Errorf(
			"unsupported spec.topology.type %q; supported values are %q, %q and %q",
			topology,
			definition.TopologyTypeStandalone,
			definition.TopologyTypeGalera,
			definition.TopologyTypeReplication,
		)
	}
	return validateTopologyImmutable(c, topology)
}

// validateStandaloneReplicas enforces that the standalone topology runs a single
// node; MariaDB has no clustering without Galera, so more than one replica is invalid.
func validateStandaloneReplicas(c *controller.Context) error {
	engine := c.Instance().Spec.Components[common.ComponentEngine]
	if engine.Replicas != nil && *engine.Replicas != standaloneDefaultReplicas {
		return fmt.Errorf(
			"standalone topology supports a single node only, got %d; use the galera or replication topology for multiple nodes",
			*engine.Replicas,
		)
	}
	return nil
}

// validateGaleraReplicas enforces Galera's quorum requirement: an odd number of
// engine nodes, at least 3.
func validateGaleraReplicas(c *controller.Context) error {
	engine := c.Instance().Spec.Components[common.ComponentEngine]
	replicas := galeraDefaultReplicas
	if engine.Replicas != nil {
		replicas = *engine.Replicas
	}
	if replicas < 3 {
		return fmt.Errorf("galera topology requires at least 3 engine replicas, got %d", replicas)
	}
	if replicas%2 == 0 {
		return fmt.Errorf(
			"galera topology requires an odd number of engine replicas to avoid split-brain, got %d",
			replicas,
		)
	}
	return nil
}

// validateReplicationReplicas enforces that the replication topology runs at
// least 2 nodes: one primary and one replica.
func validateReplicationReplicas(c *controller.Context) error {
	engine := c.Instance().Spec.Components[common.ComponentEngine]
	replicas := replicationDefaultReplicas
	if engine.Replicas != nil {
		replicas = *engine.Replicas
	}
	if replicas < 2 {
		return fmt.Errorf(
			"replication topology requires at least 2 engine replicas (one primary and one replica), got %d",
			replicas,
		)
	}
	return nil
}

// validateReplicationScaleOut rejects adding nodes to an existing replication
// Instance without a backup storage: the operator seeds new replicas from a
// physical backup of the cluster, staged in that storage.
func validateReplicationScaleOut(c *controller.Context) error {
	if replicaTemplateStorage(c.Instance()) != nil {
		return nil
	}
	existing := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(existing, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get MariaDB: %w", err)
	}
	engine := c.Instance().Spec.Components[common.ComponentEngine]
	if engine.Replicas == nil || *engine.Replicas <= existing.Spec.Replicas {
		return nil
	}
	return fmt.Errorf(
		"scaling out the %q topology requires a backup storage in spec.backup: "+
			"new replicas are seeded from a physical backup of the cluster staged there",
		definition.TopologyTypeReplication,
	)
}

// validateEngineUpdate rejects engine changes an existing cluster cannot apply.
// The previous Instance is not available at admission, so the provisioned
// MariaDB CR is the reference; nothing is checked at creation.
func validateEngineUpdate(c *controller.Context) error {
	existing := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(existing, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get MariaDB: %w", err)
	}
	engine := c.Instance().Spec.Components[common.ComponentEngine]
	if err := validateVersionDowngrade(c, engine, existing); err != nil {
		return err
	}
	return validateStorageUpdate(engine, existing)
}

// validateVersionDowngrade rejects moving the engine to an older major or minor
// version: the data directory is upgraded on version changes
// (mariaDBAutoUpgradeEnabled) and older servers cannot be relied on to read it.
// Patch-level changes, and images whose tag has no recognizable version, are
// allowed.
func validateVersionDowngrade(c *controller.Context, engine corev1alpha1.ComponentSpec, existing *mariadbv1alpha1.MariaDB) error {
	engine, err := withBundleVersion(c, engine)
	if err != nil {
		return err
	}
	image, err := resolveEngineImage(c, engine)
	if err != nil {
		return err
	}
	current, ok := imageMinorVersion(existing.Spec.Image)
	if !ok {
		return nil
	}
	desired, ok := imageMinorVersion(image)
	if !ok {
		return nil
	}
	if desired[0] < current[0] || (desired[0] == current[0] && desired[1] < current[1]) {
		return fmt.Errorf(
			"downgrading MariaDB from %d.%d to %d.%d is not supported; restore a backup into a new Instance instead",
			current[0], current[1], desired[0], desired[1],
		)
	}
	return nil
}

// withBundleVersion fills in the engine version from the Instance's version
// bundle, as the runtime does before Sync but not before admission, so the
// image validated is the one Sync will deploy.
func withBundleVersion(c *controller.Context, engine corev1alpha1.ComponentSpec) (corev1alpha1.ComponentSpec, error) {
	if engine.Image != "" || engine.Version != "" {
		return engine, nil
	}
	spec, err := c.ProviderSpec()
	if err != nil {
		return engine, fmt.Errorf("get provider spec: %w", err)
	}
	name := controller.EffectiveVersionBundleName(spec, c.Instance())
	if name == "" {
		return engine, nil
	}
	bundle, err := controller.ResolveVersionBundle(spec, name)
	if err != nil {
		return engine, err
	}
	engine.Version = bundle.Components[common.ComponentEngine]
	return engine, nil
}

var minorVersionRegexp = regexp.MustCompile(`^(\d+)\.(\d+)`)

// imageMinorVersion extracts the major and minor version from an image tag,
// e.g. "mariadb:11.4.5-ubi9" -> [11, 4].
func imageMinorVersion(image string) ([2]int, bool) {
	image, _, _ = strings.Cut(image, "@")
	i := strings.LastIndex(image, ":")
	if i < 0 || strings.Contains(image[i:], "/") {
		return [2]int{}, false
	}
	m := minorVersionRegexp.FindStringSubmatch(image[i+1:])
	if m == nil {
		return [2]int{}, false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return [2]int{major, minor}, true
}

// validateStorageUpdate rejects storage changes the operator cannot apply: a
// smaller volume (rejected by the operator, leaving the Instance stuck) and a
// different StorageClass (silently ignored, as PVCs cannot change class). A
// class set on an Instance provisioned with the default class is left as is.
func validateStorageUpdate(engine corev1alpha1.ComponentSpec, existing *mariadbv1alpha1.MariaDB) error {
	if current := existing.Spec.Storage.Size; current != nil {
		if desired := engineStorageSize(engine); desired.Cmp(*current) < 0 {
			return fmt.Errorf(
				"spec.components.%s.storage.size cannot be decreased from %s to %s",
				common.ComponentEngine, current.String(), desired.String(),
			)
		}
	}
	current := existing.Spec.Storage.StorageClassName
	if engine.Storage == nil || engine.Storage.StorageClass == nil || current == "" {
		return nil
	}
	if desired := *engine.Storage.StorageClass; desired != current {
		return fmt.Errorf(
			"spec.components.%s.storage.storageClass cannot be changed on an existing instance (current: %q)",
			common.ComponentEngine, current,
		)
	}
	return nil
}

// validateTopologyImmutable rejects switching an existing instance between
// topologies. The provider has no access to the previous Instance spec at
// admission time, so it compares against the topology of the already-provisioned
// MariaDB CR.
func validateTopologyImmutable(c *controller.Context, topology string) error {
	existing := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(existing, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get MariaDB: %w", err)
	}
	if existingTopology(existing) != normalizeTopology(topology) {
		return fmt.Errorf("spec.topology is immutable and cannot be changed on an existing instance")
	}
	return nil
}

// normalizeTopology maps an empty topology to the standalone default.
func normalizeTopology(topology string) definition.TopologyType {
	if topology == "" {
		return definition.TopologyTypeStandalone
	}
	return definition.TopologyType(topology)
}

// existingTopology derives the topology of a provisioned MariaDB CR from its HA state.
func existingTopology(mdb *mariadbv1alpha1.MariaDB) definition.TopologyType {
	switch {
	case mdb.IsGaleraEnabled():
		return definition.TopologyTypeGalera
	case mdb.IsReplicationEnabled():
		return definition.TopologyTypeReplication
	default:
		return definition.TopologyTypeStandalone
	}
}

// mustParseQuantity is a helper that panics on invalid quantity strings (compile-time constants only).
func mustParseQuantity(s string) resource.Quantity {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		panic(fmt.Sprintf("invalid quantity %q: %v", s, err))
	}
	return q
}
