package provider

// Run `make manifests` to regenerate config/rbac/role.yaml from these markers.
// This file contains kubebuilder RBAC markers for controller-gen.
// See: https://book.kubebuilder.io/reference/markers/rbac

// Base RBAC (required by all providers):
// +kubebuilder:rbac:groups=core.openeverest.io,resources=instances,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core.openeverest.io,resources=instances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.openeverest.io,resources=instances/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.openeverest.io,resources=providers,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// =============================================================================
// PROVIDER-SPECIFIC RBAC — mariadb-operator resources.
// =============================================================================

// MariaDB CR: full lifecycle management.
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=mariadbs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=mariadbs/status,verbs=get
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=mariadbs/finalizers,verbs=update

// Backup/Restore CRs — needed for Phase 5 logical backup support.
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=backups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=backups/status,verbs=get
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=restores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=restores/status,verbs=get

// PhysicalBackup CRs — physical backup support.
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=physicalbackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=physicalbackups/status,verbs=get

// PointInTimeRecovery CRs — point-in-time recovery (binary log archival) support.
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=pointintimerecoveries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=pointintimerecoveries/status,verbs=get

// MaxScale CRs — proxy component.
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=maxscales,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=maxscales/status,verbs=get

// VMServiceScrape CRs — VictoriaMetrics scraping of the metrics exporter.
// +kubebuilder:rbac:groups=operator.victoriametrics.com,resources=vmservicescrapes,verbs=get;list;watch;create;update;patch;delete

// =============================================================================
// OPENEVEREST BACKUP RESOURCES
// =============================================================================
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backupclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backupstorages,verbs=get;list;watch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=restores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=restores/status,verbs=get;update;patch

// Kubernetes core resources: Secrets (credentials), Services (connection details).
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch

// Pods — the runtime counts the labelled component pods into status.components.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Jobs — mirror scheduled backup runs (CronJob-produced Jobs) into Backup CRs.
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch
