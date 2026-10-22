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
	"errors"
	"fmt"
	"strconv"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/definition/components"
	"github.com/openeverest/provider-mariadb/internal/common"
)

const (
	// defaultPort is the default MariaDB port.
	defaultPort = 3306

	// rootPasswordSecretKey is the key in the root password Secret.
	rootPasswordSecretKey = "root-password"

	// userPasswordSecretKey is the key in the initial user's password Secret.
	userPasswordSecretKey = "password"

	// defaultInitialDatabase is the initial database created on the cluster.
	defaultInitialDatabase = "everest"

	// defaultInitialUser is the initial user created on the cluster.
	defaultInitialUser = "everest"
)

var errTLSCABundleNotReady = errors.New("TLS CA bundle is not ready")

// userSecretName returns the name of the Secret holding the initial user's password
// for the given instance. Follows the OpenEverest convention used across providers and
// is the Secret surfaced through the connection details.
func userSecretName(instanceName string) string {
	return "everest-secrets-" + instanceName
}

// rootSecretName returns the name of the Secret holding the root password for the given
// instance. The root password lives in a dedicated Secret because the operator generates
// each password Secret only when the whole Secret is absent, so root and user passwords
// cannot share a single Secret.
func rootSecretName(instanceName string) string {
	return "everest-secrets-" + instanceName + "-root"
}

// SyncMariaDB server-side applies the MariaDB CR built from the Instance spec.
// Only the fields the provider owns are declared; the operator keeps owning the
// ones it defaults. Fields the provider stops declaring are removed.
func SyncMariaDB(c *controller.Context) error {
	l := log.FromContext(c.Context())
	l.Info("Syncing MariaDB cluster", "cluster", c.Name())
	defer l.Info("MariaDB cluster synced", "cluster", c.Name())

	desired, err := desiredMariaDB(c)
	if err != nil {
		return err
	}

	existing := &mariadbv1alpha1.MariaDB{}
	err = c.Get(existing, c.Name())
	switch {
	case apierrors.IsNotFound(err):
		// Seeding from spec.dataSource is only possible into a fresh MariaDB
		// via bootstrapFrom, so it is resolved at creation time only.
		bootstrap, sourceInstance, err := resolveBootstrapFrom(c)
		if err != nil {
			return err
		}
		if bootstrap != nil {
			// The backup embeds the source's credentials; copy them so the
			// operator does not generate mismatched passwords that would fail
			// the probes and crash-loop the restored Pods.
			if err := ensureRestoreCredentials(c, sourceInstance); err != nil {
				return err
			}
		}
		desired.Spec.BootstrapFrom = bootstrap
	case err != nil:
		return fmt.Errorf("get MariaDB: %w", err)
	default:
		if err := migrateFieldOwnership(c, existing); err != nil {
			return err
		}
		// Immutable once set: keep declaring them so the apply never removes them.
		desired.Spec.BootstrapFrom = existing.Spec.BootstrapFrom
		desired.Spec.Storage.StorageClassName = existing.Spec.Storage.StorageClassName
	}

	obj, err := toApplyObject(c, desired)
	if err != nil {
		return err
	}
	if err := c.Apply(obj); err != nil {
		return fmt.Errorf("apply MariaDB: %w", err)
	}
	return nil
}

// desiredMariaDB builds the MariaDB fields the provider owns from the Instance
// spec. bootstrapFrom is set by the caller, as it depends on existing state.
func desiredMariaDB(c *controller.Context) (*mariadbv1alpha1.MariaDB, error) {
	engine := c.Instance().Spec.Components[common.ComponentEngine]

	image, err := resolveEngineImage(c, engine)
	if err != nil {
		return nil, err
	}

	galera := isGaleraTopology(c)
	replication := isReplicationTopology(c)
	ha := galera || replication

	replicas := defaultReplicas(c)
	if engine.Replicas != nil {
		replicas = *engine.Replicas
	}

	storage := mariadbv1alpha1.Storage{
		Size:      ptr.To(engineStorageSize(engine)),
		Ephemeral: ptr.To(false),
	}
	if engine.Storage != nil {
		if engine.Storage.StorageClass != nil {
			storage.StorageClassName = *engine.Storage.StorageClass
		}
	}

	var resourceReqs *mariadbv1alpha1.ResourceRequirements
	if engine.Resources != nil && (engine.Resources.Limits != nil || engine.Resources.Requests != nil) {
		resourceReqs = &mariadbv1alpha1.ResourceRequirements{
			Limits:   engine.Resources.Limits,
			Requests: engine.Resources.Requests,
		}
	}

	// Optional my.cnf from the engine component's `configuration` parameter.
	var myCnf *string
	var params components.MariadbParameters
	if c.TryDecodeComponentParameters(engine, &params) && params.Configuration != "" {
		myCnf = &params.Configuration
	}

	// Nil when monitoring is not enabled, so the operator deploys no exporter.
	metrics, err := buildMetrics(c)
	if err != nil {
		return nil, fmt.Errorf("build metrics: %w", err)
	}

	// The user's affinity, or for HA topologies a soft pod anti-affinity that
	// spreads nodes without blocking scheduling. AntiAffinityEnabled stays unset
	// so the operator does not default a competing affinity.
	scheduling := ptr.Deref(engine.SchedulingPolicy, commonv1alpha1.SchedulingPolicy{})
	affinity := buildAffinity(scheduling.Affinity, ha, c.Name())

	// Only the TLS switches are declared, so the operator's generated
	// certificate and CA references stay with the operator.
	tls, err := buildTLS(c)
	if err != nil {
		return nil, fmt.Errorf("build TLS: %w", err)
	}

	// Binary log archival is turned on by referencing the PointInTimeRecovery
	// CR once it exists; the CR itself is reconciled by SyncPITR.
	pitrRef, err := desiredPITRRef(c)
	if err != nil {
		return nil, fmt.Errorf("resolve PITR reference: %w", err)
	}

	// spec.maxScaleRef is never declared: MaxScale only routes traffic and the
	// operator keeps owning failover (#37).
	mdb := &mariadbv1alpha1.MariaDB{
		ObjectMeta: c.ObjectMeta(c.Name()),
		Spec: mariadbv1alpha1.MariaDBSpec{
			Image:                    image,
			ImagePullPolicy:          corev1.PullIfNotPresent,
			Replicas:                 replicas,
			Storage:                  storage,
			RootEmptyPassword:        ptr.To(false),
			MyCnf:                    myCnf,
			RootPasswordSecretKeyRef: generatedSecretKeyRef(rootSecretName(c.Name()), rootPasswordSecretKey),
			Username:                 ptr.To(defaultInitialUser),
			Database:                 ptr.To(defaultInitialDatabase),
			PasswordSecretKeyRef:     ptr.To(generatedSecretKeyRef(userSecretName(c.Name()), userPasswordSecretKey)),
			ContainerTemplate: mariadbv1alpha1.ContainerTemplate{
				Resources: resourceReqs,
			},
			Metrics:                metrics,
			TLS:                    tls,
			PointInTimeRecoveryRef: pitrRef,
			// Keeps the agent/init images in lockstep with the bundled operator,
			// and runs mariadb-upgrade on start so major version changes migrate
			// the system schema.
			UpdateStrategy: mariadbv1alpha1.UpdateStrategy{
				AutoUpdateDataPlane:       ptr.To(true),
				MariaDBAutoUpgradeEnabled: ptr.To(true),
			},
		},
	}
	mdb.Spec.Affinity = affinity
	mdb.Spec.NodeSelector = scheduling.NodeSelector
	mdb.Spec.Tolerations = scheduling.Tolerations
	mdb.Spec.TopologySpreadConstraints = convertTopologySpreadConstraints(
		controller.TopologySpreadConstraints(&scheduling, mariadbPodLabels(c.Name())),
	)
	// The operator adds these to the pod template only, never to the selectors.
	mdb.Spec.PodMetadata = &mariadbv1alpha1.Metadata{Labels: c.PodLabels(common.ComponentEngine)}
	// InheritMetadata, unlike PodMetadata, is copied onto every child resource
	// the operator creates, including the metrics ServiceMonitor. That's what
	// lets plugin-metrics find it: it matches on this instance label, not on
	// anything pod-specific.
	mdb.Spec.InheritMetadata = &mariadbv1alpha1.Metadata{
		Labels: map[string]string{controller.InstanceLabel: c.Name()},
	}
	if galera {
		mdb.Spec.Galera = &mariadbv1alpha1.Galera{Enabled: true}
	}
	if replication {
		mdb.Spec.Replication = &mariadbv1alpha1.Replication{
			Enabled: true,
			// A restarted former primary must not boot writable: until the operator
			// demotes it, writes routed to it would diverge from the new primary.
			SemiSyncBootAsReplica: ptr.To(true),
		}
		mdb.Spec.Replication.Replica.ReplicaBootstrapFrom = desiredReplicaBootstrapFrom(c)
		if mdb.Spec.Replication.Replica.ReplicaBootstrapFrom != nil {
			// Rebuild replicas that break (e.g. the primary purged binary logs
			// they still need) from a fresh physical backup of the cluster.
			mdb.Spec.Replication.Replica.ReplicaRecovery = &mariadbv1alpha1.ReplicaRecovery{Enabled: true}
		}
	}

	// Standalone routes clients to the general Service (<name>); HA topologies
	// route writes to the primary Service (<name>-primary).
	if ha {
		mdb.Spec.PrimaryService = configureService(engine.Service)
	} else {
		mdb.Spec.Service = configureService(engine.Service)
	}
	return mdb, nil
}

// engineStorageSize returns the data volume size requested for the engine, or
// the provider default.
func engineStorageSize(engine corev1alpha1.ComponentSpec) resource.Quantity {
	if engine.Storage != nil && !engine.Storage.Size.IsZero() {
		return engine.Storage.Size
	}
	return resource.MustParse("10Gi")
}

// resolveEngineImage returns the engine image from the component override, its
// selected version, or the provider default.
func resolveEngineImage(c *controller.Context, engine corev1alpha1.ComponentSpec) (string, error) {
	if engine.Image != "" {
		return engine.Image, nil
	}
	spec, err := c.ProviderSpec()
	if err != nil {
		return "", fmt.Errorf("get provider spec: %w", err)
	}
	image := ""
	if engine.Version != "" {
		image = controller.GetImageForVersion(spec, common.ComponentEngine, engine.Version)
	}
	if image == "" {
		image = controller.GetDefaultImageForComponent(spec, common.ComponentEngine)
	}
	return image, nil
}

func generatedSecretKeyRef(name, key string) mariadbv1alpha1.GeneratedSecretKeyRef {
	return mariadbv1alpha1.GeneratedSecretKeyRef{
		SecretKeySelector: mariadbv1alpha1.SecretKeySelector{
			LocalObjectReference: mariadbv1alpha1.LocalObjectReference{Name: name},
			Key:                  key,
		},
		Generate: true,
	}
}

// configureService maps the SDK Service exposure request onto the operator's
// ServiceTemplate. Returns nil when no exposure is requested (operator defaults
// to a ClusterIP Service).
func configureService(svc *corev1alpha1.Service) *mariadbv1alpha1.ServiceTemplate {
	if svc == nil {
		return nil
	}

	serviceType := svc.ServiceType
	if serviceType == "" {
		serviceType = corev1.ServiceTypeClusterIP
	}

	tmpl := &mariadbv1alpha1.ServiceTemplate{
		Type: serviceType,
	}
	if len(svc.Annotations) > 0 {
		tmpl.Metadata = &mariadbv1alpha1.Metadata{Annotations: svc.Annotations}
	}
	if serviceType == corev1.ServiceTypeLoadBalancer &&
		svc.LoadBalancerService != nil &&
		svc.LoadBalancerService.SourceRanges != nil {
		tmpl.LoadBalancerSourceRanges = svc.LoadBalancerService.SourceRanges.NormalizedSourceRanges()
	}
	return tmpl
}

// StatusMariaDB reads the MariaDB CR status and translates it to an Instance status.
func StatusMariaDB(c *controller.Context) (controller.Status, error) {
	mariadbCR := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(mariadbCR, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.Provisioning("Waiting for MariaDB cluster to be created"), nil
		}
		return controller.Status{}, fmt.Errorf("get MariaDB: %w", err)
	}

	// Seeding from spec.dataSource owns the phase until it reaches a terminal
	// state: a logical restore runs while the MariaDB is already Ready, and the
	// Instance must not expose connection details for an empty database.
	if ds := c.GetDataSourceStatus(); ds != nil {
		if ds.State == controller.DataSourceStateFailed {
			return controller.Failed(ds.Message), nil
		}
		if !ds.Done {
			return controller.Restoring(ds.Message), nil
		}
	}

	// Check the Ready condition.
	if mariadbCR.IsReady() {
		proxyEnabled, err := isProxyEnabled(c)
		if err != nil {
			return controller.Status{}, err
		}
		var mxs *mariadbv1alpha1.MaxScale
		if proxyEnabled {
			ready, msg, err := readyMaxScale(c)
			if err != nil {
				return controller.Status{}, err
			}
			if ready == nil {
				return controller.Provisioning(msg), nil
			}
			mxs = ready
		}

		details, err := buildConnectionDetails(c, mxs)
		if err != nil {
			if errors.Is(err, errTLSCABundleNotReady) {
				return controller.Provisioning("Waiting for MariaDB TLS CA bundle"), nil
			}
			if apierrors.IsNotFound(err) {
				return controller.Provisioning("Waiting for MariaDB credentials secret"), nil
			}
			return controller.Status{}, err
		}
		return controller.ReadyWithConnectionDetails(details), nil
	}

	// Extract a useful message from the Ready condition if available.
	if cond, ok := getReadyCondition(mariadbCR); ok && cond.Message != "" {
		return controller.Provisioning(cond.Message), nil
	}

	return controller.Provisioning("Waiting for MariaDB cluster to be ready"), nil
}

// buildConnectionDetails reads the generated user credentials secret and combines
// it with the client-facing Service host to produce a full set of connection
// details. When mxs is set, clients are routed through MaxScale; it
// authenticates them against the same MariaDB users and listens on the same port.
func buildConnectionDetails(c *controller.Context, mxs *mariadbv1alpha1.MaxScale) (controller.ConnectionDetails, error) {
	secret := &corev1.Secret{}
	if err := c.Get(secret, userSecretName(c.Name())); err != nil {
		return controller.ConnectionDetails{}, fmt.Errorf("get credentials secret: %w", err)
	}

	serviceName := c.Name()
	if isHATopology(c) {
		serviceName = c.Name() + primaryServiceSuffix
	}
	caBundleSecretName := tlsCABundleSecretName(c.Name())
	var tlsEnabled bool
	if mxs != nil {
		serviceName = mxs.Name
		caBundleSecretName = tlsCABundleSecretName(mxs.Name)
		tlsEnabled = mxs.IsTLSEnabled()
	} else {
		tlsSettings, err := resolveTLSSettings(c)
		if err != nil {
			return controller.ConnectionDetails{}, fmt.Errorf("resolve TLS settings: %w", err)
		}
		tlsEnabled = tlsSettings.Enabled
	}

	host := resolveHost(c, serviceName)
	port := strconv.Itoa(defaultPort)
	username := defaultInitialUser
	password := string(secret.Data[userPasswordSecretKey])
	additionalProperties := map[string]string{}

	if tlsEnabled {
		caSecret := &corev1.Secret{}
		if err := c.Get(caSecret, caBundleSecretName); err != nil {
			return controller.ConnectionDetails{}, fmt.Errorf("%w: %v", errTLSCABundleNotReady, err)
		}
		ca, ok := caSecret.Data[tlsCAKey]
		if !ok || len(ca) == 0 {
			return controller.ConnectionDetails{}, fmt.Errorf("%w: secret %q has no %q key", errTLSCABundleNotReady, caSecret.Name, tlsCAKey)
		}
		additionalProperties[tlsConnectionKey] = strconv.FormatBool(true)
		additionalProperties[tlsCAKey] = string(ca)
	}

	return controller.ConnectionDetails{
		Type:     "mysql",
		Provider: common.ProviderName,
		Host:     host,
		Port:     port,
		Username: username,
		Password: password,
		URI: fmt.Sprintf(
			"mysql://%s:%s@%s:%s/%s",
			username, password, host, port, defaultInitialDatabase,
		),
		AdditionalProperties: additionalProperties,
	}, nil
}

// resolveHost returns the externally reachable host for the given client-facing
// Service. It prefers a LoadBalancer ingress address when available, otherwise
// the internal cluster FQDN.
func resolveHost(c *controller.Context, serviceName string) string {
	internal := fmt.Sprintf("%s.%s.svc", serviceName, c.Namespace())

	svc := &corev1.Service{}
	if err := c.Get(svc, serviceName); err != nil {
		return internal
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return internal
	}
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			return ing.IP
		}
		if ing.Hostname != "" {
			return ing.Hostname
		}
	}
	return internal
}

// CleanupMariaDB deletes the MaxScale and MariaDB CRs when the Instance is being
// deleted. Both are owned by the Instance (via c.ObjectMeta), so cascaded GC
// handles child resources automatically. This explicit delete ensures the operator
// performs any finalizer-driven cleanup (e.g., releasing PVCs via operator policy).
// MaxScale goes first so it stops managing the servers before they disappear.
func CleanupMariaDB(c *controller.Context) error {
	l := log.FromContext(c.Context())
	l.Info("Cleaning up MariaDB cluster", "cluster", c.Name())

	if err := teardownMaxScale(c); err != nil {
		return err
	}

	mariadbCR := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.Name(),
			Namespace: c.Namespace(),
		},
	}
	if err := c.Delete(mariadbCR); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete MariaDB: %w", err)
	}

	return nil
}

// getReadyCondition returns the Ready condition from the MariaDB status, if present.
func getReadyCondition(m *mariadbv1alpha1.MariaDB) (metav1.Condition, bool) {
	for _, cond := range m.Status.Conditions {
		if cond.Type == mariadbv1alpha1.ConditionTypeReady {
			return cond, true
		}
	}
	return metav1.Condition{}, false
}
