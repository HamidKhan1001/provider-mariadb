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

// package provider implements the OpenEverest provider for MariaDB,
// backed by the mariadb-operator (k8s.mariadb.com API group).
package provider

import (
	"context"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/internal/common"
)

// Compile-time checks that MariaDBProvider implements the required interfaces.
var (
	_ controller.ProviderInterface = (*MariaDBProvider)(nil)
	_ controller.BackupProvider    = (*MariaDBProvider)(nil)
	_ controller.BackupWatcher     = (*MariaDBProvider)(nil)
	_ controller.RestoreWatcher    = (*MariaDBProvider)(nil)
	_ controller.BackupMirror      = (*MariaDBProvider)(nil)
)

// MariaDBProvider implements controller.ProviderInterface for MariaDB via mariadb-operator.
type MariaDBProvider struct {
	controller.BaseProvider
}

// New creates a new MariaDBProvider instance.
func New() *MariaDBProvider {
	return &MariaDBProvider{
		BaseProvider: controller.BaseProvider{
			ProviderName: common.ProviderName,
			SchemeFuncs: []func(*runtime.Scheme) error{
				mariadbv1alpha1.AddToScheme,
				// Registered so the Backup mirror can watch run Jobs.
				batchv1.AddToScheme,
			},
			WatchConfigs: []controller.WatchConfig{
				// Re-enqueue the Instance when its owned MariaDB CR changes
				// (e.g., operator updates status after reconciliation).
				controller.WatchOwned(&mariadbv1alpha1.MariaDB{}),
				// Re-enqueue when the PITR CR status changes so the recovery
				// window surfaced on the Instance stays current.
				controller.WatchOwned(&mariadbv1alpha1.PointInTimeRecovery{}),
				// Re-enqueue when MaxScale changes so the Instance status
				// tracks proxy readiness.
				controller.WatchOwned(&mariadbv1alpha1.MaxScale{}),
			},
		},
	}
}

// Validate checks if the Instance spec is valid before a create/update is accepted.
// Called by the validation webhook.
func (p *MariaDBProvider) Validate(c *controller.Context) error {
	return ValidateMariaDB(c)
}

// Sync ensures the MariaDB CR exists and reflects the current Instance spec.
func (p *MariaDBProvider) Sync(c *controller.Context) error {
	if err := SyncMariaDB(c); err != nil {
		return err
	}
	if err := SyncReplicaTemplate(c); err != nil {
		return err
	}
	if err := SyncMaxScale(c); err != nil {
		return err
	}
	if err := SyncVMServiceScrape(c); err != nil {
		return err
	}
	if err := SyncScheduledBackups(c); err != nil {
		return err
	}
	if err := SyncPITR(c); err != nil {
		return err
	}
	return syncDataSourceStatus(c)
}

// Status reads the MariaDB CR status and translates it to an Instance status.
func (p *MariaDBProvider) Status(c *controller.Context) (controller.Status, error) {
	return StatusMariaDB(c)
}

// Cleanup deletes the MaxScale and MariaDB CRs when the Instance is deleted.
// Owner references handle cascaded cleanup of child resources.
func (p *MariaDBProvider) Cleanup(c *controller.Context) error {
	return CleanupMariaDB(c)
}

// BackupWatches wires the runtime's Backup reconciler to watch operator Backup
// CRs as owned resources so operator status changes route directly to the
// parent Backup CR via owner-reference based enqueue. SyncBackup sets the
// controller reference from Backup -> operator Backup. It also watches the run
// Jobs of scheduled backups so mirrored Backup CRs pick up completion status.
func (p *MariaDBProvider) BackupWatches() []controller.WatchConfig {
	return []controller.WatchConfig{
		controller.WatchOwned(&mariadbv1alpha1.Backup{}),
		controller.WatchOwned(&mariadbv1alpha1.PhysicalBackup{}),
		controller.WatchExternal(
			&batchv1.Job{},
			handler.EnqueueRequestsFromMapFunc(scheduledRunToBackupRequest),
			controller.ResourceVersionChangedPredicate,
		),
	}
}

// scheduledRunToBackupRequest maps a scheduled backup run Job to the mirrored
// Backup CR that shares its name, and ignores unrelated Jobs.
func scheduledRunToBackupRequest(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetLabels()[scheduleLabel] == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()},
	}}
}

// RestoreWatches mirrors BackupWatches for operator Restore CRs.
func (p *MariaDBProvider) RestoreWatches() []controller.WatchConfig {
	return []controller.WatchConfig{
		controller.WatchOwned(&mariadbv1alpha1.Restore{}),
	}
}
