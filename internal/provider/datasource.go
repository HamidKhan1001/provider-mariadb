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

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// resolveBootstrapFrom builds the operator MariaDB.spec.bootstrapFrom
// used to seed a brand-new Instance from spec.dataSource. It returns:
//   - (nil, "", nil) when the Instance has no DataSource;
//   - a WaitFor error while the source is not yet resolvable or ready;
//   - a populated BootstrapFrom plus the source Instance name once the source
//     is ready.
//
// bootstrapFrom is immutable, so this is only consulted when the MariaDB CR is
// first created.
func resolveBootstrapFrom(c *controller.Context) (*mariadbv1alpha1.BootstrapFrom, string, error) {
	ds := c.Instance().Spec.DataSource
	if ds == nil {
		return nil, "", nil
	}
	switch ds.Type {
	case backupv1alpha1.DataSourceTypeBackup:
		return resolveBackupBootstrapFrom(c, ds)
	case backupv1alpha1.DataSourceTypePointInTime:
		return resolvePointInTimeBootstrapFrom(c, ds)
	default:
		return nil, "", nil
	}
}

// resolveBackupBootstrapFrom seeds from the logical or physical Backup
// referenced by spec.dataSource.backup. Physical backups are restored before
// MariaDB starts; logical backups are restored by the operator once the new
// cluster is up, which keeps it unready until the restore completes.
func resolveBackupBootstrapFrom(
	c *controller.Context,
	ds *backupv1alpha1.DataSource,
) (*mariadbv1alpha1.BootstrapFrom, string, error) {
	if ds.Backup == nil {
		return nil, "", nil
	}

	sourceBackup := &backupv1alpha1.Backup{}
	if err := c.Get(sourceBackup, ds.Backup.BackupRef.Name); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", controller.WaitFor(
				fmt.Sprintf("Waiting for source Backup %q", ds.Backup.BackupRef.Name))
		}
		return nil, "", fmt.Errorf("get source Backup: %w", err)
	}
	params, err := parseBackupParams(rawParameters(sourceBackup.Spec.Parameters))
	if err != nil {
		return nil, "", err
	}
	contentType := mariadbv1alpha1.BackupContentTypeLogical
	if isPhysical(params) {
		contentType = mariadbv1alpha1.BackupContentTypePhysical
	}
	if sourceBackup.Status.State == backupv1alpha1.BackupStateFailed {
		return nil, "", &controller.DataSourceConfigError{
			Reason:  corev1alpha1.ReasonDataSourceSourceBackupNotSucceeded,
			Message: fmt.Sprintf("source Backup %q failed; cannot seed", sourceBackup.Name),
		}
	}
	if sourceBackup.Status.State != backupv1alpha1.BackupStateSucceeded {
		return nil, "", controller.WaitFor(
			fmt.Sprintf("Waiting for source Backup %q to succeed", sourceBackup.Name))
	}

	sourceInstance := sourceBackup.Spec.Origin.InstanceRef.Name
	// Read the backup files from the source Instance's prefix in the storage it
	// was written to. Logical and physical backups share the prefix; the
	// operator only considers files of the requested content type.
	s3, err := buildOperatorS3(c, sourceBackup.Spec.StorageRef.Name, sourceInstance)
	if err != nil {
		return nil, "", err
	}
	bootstrap := &mariadbv1alpha1.BootstrapFrom{
		S3:                s3,
		BackupContentType: contentType,
	}
	// Pin selection to the specific source backup: the operator matches the
	// closest backup at or before this time.
	if sourceBackup.Status.CompletedAt != nil {
		bootstrap.TargetRecoveryTime = sourceBackup.Status.CompletedAt
	}
	return bootstrap, sourceInstance, nil
}

// resolvePointInTimeBootstrapFrom seeds a new Instance by restoring the source
// Instance's PITR base backup and replaying archived binary logs up to the
// recovery target. The source Instance must be named explicitly (a new Instance
// has no stream of its own) and must have PITR configured.
func resolvePointInTimeBootstrapFrom(
	c *controller.Context,
	ds *backupv1alpha1.DataSource,
) (*mariadbv1alpha1.BootstrapFrom, string, error) {
	pit := ds.PointInTime
	if pit == nil {
		return nil, "", nil
	}
	if pit.Source.InstanceRef == nil || pit.Source.InstanceRef.Name == "" {
		return nil, "", &controller.DataSourceConfigError{
			Reason:  corev1alpha1.ReasonDataSourceFailed,
			Message: "dataSource.pointInTime.source.instanceRef is required to seed a new Instance",
		}
	}
	if pit.RecoveryTarget == backupv1alpha1.RecoveryTargetDate && pit.Date == nil {
		return nil, "", &controller.DataSourceConfigError{
			Reason:  corev1alpha1.ReasonDataSourceFailed,
			Message: "dataSource.pointInTime.date is required when recoveryTarget is \"date\"",
		}
	}

	sourceInstance := pit.Source.InstanceRef.Name
	pitrName := pitrCRName(sourceInstance)
	pitr := &mariadbv1alpha1.PointInTimeRecovery{}
	if err := c.Get(pitr, pitrName); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", controller.WaitFor(
				fmt.Sprintf("Waiting for source PointInTimeRecovery %q", pitrName))
		}
		return nil, "", fmt.Errorf("get source PointInTimeRecovery: %w", err)
	}

	bootstrap := &mariadbv1alpha1.BootstrapFrom{
		PointInTimeRecoveryRef: &mariadbv1alpha1.LocalObjectReference{Name: pitrName},
		// PITR restores a physical base; mark it so the provider's data-source
		// readiness logic recognizes the seeded cluster.
		BackupContentType: mariadbv1alpha1.BackupContentTypePhysical,
	}
	// A "latest" target leaves TargetRecoveryTime unset so the operator replays
	// up to the last recoverable time.
	if pit.RecoveryTarget == backupv1alpha1.RecoveryTargetDate {
		t := metav1.NewTime(pit.Date.UTC())
		bootstrap.TargetRecoveryTime = &t
	}
	return bootstrap, sourceInstance, nil
}

// ensureRestoreCredentials copies the source Instance's root and user
// credential Secrets onto the restored Instance's Secret names before the
// operator creates the MariaDB CR. Physical backups, and logical backups taken
// without ignoreGlobalPriv, embed the source's mysql.global_priv table, so the
// restored cluster must reuse the source credentials; otherwise the operator
// would generate fresh random passwords and the liveness/readiness probes
// (which authenticate as root) would fail, crash-looping the Pods. It is
// idempotent — existing target Secrets are left untouched — and must run before
// the CR is applied.
func ensureRestoreCredentials(c *controller.Context, sourceInstance string) error {
	if err := copyInstanceSecret(c, rootSecretName(sourceInstance), rootSecretName(c.Name())); err != nil {
		return err
	}
	return copyInstanceSecret(c, userSecretName(sourceInstance), userSecretName(c.Name()))
}

// copyInstanceSecret creates dstName as a copy of srcName's data, owned by the
// Instance so it is garbage-collected with it. Existing destinations are left
// as-is. A missing source is treated as a terminal seeding failure since the
// restored data cannot be authenticated without it.
func copyInstanceSecret(c *controller.Context, srcName, dstName string) error {
	dst := &corev1.Secret{}
	if err := c.Get(dst, dstName); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get target Secret %q: %w", dstName, err)
	}

	src := &corev1.Secret{}
	if err := c.Get(src, srcName); err != nil {
		if apierrors.IsNotFound(err) {
			return &controller.DataSourceConfigError{
				Reason: corev1alpha1.ReasonDataSourceFailed,
				Message: fmt.Sprintf(
					"source Instance credentials Secret %q not found; the source Instance may have been deleted", srcName),
			}
		}
		return fmt.Errorf("get source Secret %q: %w", srcName, err)
	}

	newSecret := &corev1.Secret{
		ObjectMeta: c.ObjectMeta(dstName),
		Type:       src.Type,
		Data:       src.Data,
	}
	if err := controllerutil.SetControllerReference(c.Instance(), newSecret, c.Client().Scheme()); err != nil {
		return fmt.Errorf("set owner reference on Secret %q: %w", dstName, err)
	}
	if err := c.Client().Create(c.Context(), newSecret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create target Secret %q: %w", dstName, err)
	}
	return nil
}

// syncDataSourceStatus stages the DataSourceReady status for an Instance
// seeded via MariaDB.spec.bootstrapFrom, translating the seeded MariaDB's state
// into the runtime's DataSourceStatus. It is a no-op for Instances that are not
// seeded. Once seeding succeeds the runtime keeps reporting it as succeeded.
func syncDataSourceStatus(c *controller.Context) error {
	ds := c.Instance().Spec.DataSource
	if ds == nil {
		return nil
	}

	mdb := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(mdb, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get MariaDB: %w", err)
	}
	if mdb.Spec.BootstrapFrom == nil {
		return nil
	}

	switch mdb.Spec.BootstrapFrom.BackupContentType {
	case mariadbv1alpha1.BackupContentTypePhysical:
		if mdb.IsReady() {
			c.SetDataSourceStatus(dataSourceSucceeded("Instance seeded from physical backup"))
			return nil
		}
		c.SetDataSourceStatus(dataSourceRestoring("Restoring physical backup into new MariaDB cluster"))
	case mariadbv1alpha1.BackupContentTypeLogical:
		// The operator only restores a logical backup once the new cluster is
		// up, so readiness alone does not mean the data is in place.
		if mdb.HasRestoredBackup() && mdb.IsReady() {
			c.SetDataSourceStatus(dataSourceSucceeded("Instance seeded from logical backup"))
			return nil
		}
		failed, msg, err := bootstrapRestoreFailed(c, mdb)
		if err != nil {
			return err
		}
		if failed {
			c.SetDataSourceStatus(controller.DataSourceStatus{
				Done:    true,
				State:   controller.DataSourceStateFailed,
				Reason:  corev1alpha1.ReasonDataSourceFailed,
				Message: "Restoring logical backup failed: " + msg,
			})
			return nil
		}
		c.SetDataSourceStatus(dataSourceRestoring("Restoring logical backup into new MariaDB cluster"))
	}
	return nil
}

// bootstrapRestoreFailed reports whether the operator Restore that applies a
// logical bootstrapFrom has failed, along with its failure message.
func bootstrapRestoreFailed(c *controller.Context, mdb *mariadbv1alpha1.MariaDB) (bool, string, error) {
	restore := &mariadbv1alpha1.Restore{}
	if err := c.Get(restore, mdb.RestoreKey().Name); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("get MariaDB restore: %w", err)
	}
	state := mapRestoreState(restore.Status.Conditions)
	return state.State == backupv1alpha1.RestoreStateFailed, state.Message, nil
}

func dataSourceSucceeded(msg string) controller.DataSourceStatus {
	return controller.DataSourceStatus{
		Done:    true,
		State:   controller.DataSourceStateSucceeded,
		Reason:  corev1alpha1.ReasonDataSourceSucceeded,
		Message: msg,
	}
}

func dataSourceRestoring(msg string) controller.DataSourceStatus {
	return controller.DataSourceStatus{
		Done:    false,
		State:   controller.DataSourceStateRestoring,
		Reason:  corev1alpha1.ReasonDataSourceRestoring,
		Message: msg,
	}
}
