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
	"encoding/json"
	"fmt"
	"strings"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	apicommon "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	mariadbbackup "github.com/openeverest/provider-mariadb/definition/backupclasses/mariadb"
)

const (
	// s3AccessKeyIDSecretKey is the Secret key holding the S3 access key id in
	// a BackupStorage credentials Secret.
	s3AccessKeyIDSecretKey = "AWS_ACCESS_KEY_ID"
	// s3SecretAccessKeySecretKey is the Secret key holding the S3 secret access
	// key in a BackupStorage credentials Secret.
	s3SecretAccessKeySecretKey = "AWS_SECRET_ACCESS_KEY"

	// backupTypeLogical selects a logical (mariadb-dump) backup.
	backupTypeLogical = "logical"
	// backupTypePhysical selects a physical (mariadb-backup) backup.
	backupTypePhysical = "physical"
)

// parseBackupParams decodes structured backup parameters (from a Backup CR or a
// schedule). An absent payload yields the zero value (which is logical).
func parseBackupParams(raw []byte) (mariadbbackup.MariadbBackupParameters, error) {
	var params mariadbbackup.MariadbBackupParameters
	if len(raw) == 0 {
		return params, nil
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return params, &controller.BackupConfigError{
			Reason:  "InvalidParameters",
			Message: fmt.Sprintf("decode backup parameters: %v", err),
		}
	}
	return params, nil
}

// isPhysical reports whether the parameters select a physical backup. An empty
// type defaults to logical.
func isPhysical(params mariadbbackup.MariadbBackupParameters) bool {
	return params.Type == backupTypePhysical
}

// rawParameters returns the raw JSON of a RawExtension, or nil when absent.
func rawParameters(p *runtime.RawExtension) []byte {
	if p == nil {
		return nil
	}
	return p.Raw
}

// buildOperatorS3 resolves the named BackupStorage into the operator's S3 spec,
// isolating objects under the given prefix (normally the owning Instance name,
// or the source Instance name when bootstrapping a restore). Only S3-compatible
// storages are supported; anything else is rejected as a configuration error.
// It is shared by logical (Backup) and physical (PhysicalBackup) backups, which
// wrap the same S3 spec in different storage types.
func buildOperatorS3(c *controller.Context, storageName, prefix string) (*mariadbv1alpha1.S3, error) {
	bs, err := c.BackupStorage(storageName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, controller.WaitFor(
				fmt.Sprintf("BackupStorage %q not yet present", storageName))
		}
		return nil, err
	}
	if bs.Spec.S3 == nil {
		return nil, &controller.BackupConfigError{
			Reason:  "UnsupportedStorage",
			Message: fmt.Sprintf("BackupStorage %q is not S3-compatible", storageName),
		}
	}
	s3 := bs.Spec.S3

	// The operator expects the endpoint without a scheme and derives TLS from
	// the dedicated flag below.
	endpoint := s3.EndpointURL
	useTLS := strings.HasPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")

	credentialsSecret := s3.CredentialsSecretRef.Name
	return &mariadbv1alpha1.S3{
		Bucket:   s3.Bucket,
		Endpoint: endpoint,
		Region:   s3.Region,
		Prefix:   prefix,
		AccessKeyIdSecretKeyRef: &mariadbv1alpha1.SecretKeySelector{
			LocalObjectReference: mariadbv1alpha1.LocalObjectReference{Name: credentialsSecret},
			Key:                  s3AccessKeyIDSecretKey,
		},
		SecretAccessKeySecretKeyRef: &mariadbv1alpha1.SecretKeySelector{
			LocalObjectReference: mariadbv1alpha1.LocalObjectReference{Name: credentialsSecret},
			Key:                  s3SecretAccessKeySecretKey,
		},
		TLS: &mariadbv1alpha1.TLSConfig{Enabled: useTLS},
	}, nil
}

// buildOperatorS3Storage wraps the resolved S3 spec in the operator's logical
// Backup storage type, isolating the Instance's backups under its own prefix.
func buildOperatorS3Storage(c *controller.Context, storageName string) (mariadbv1alpha1.BackupStorage, error) {
	s3, err := buildOperatorS3(c, storageName, c.Name())
	if err != nil {
		return mariadbv1alpha1.BackupStorage{}, err
	}
	return mariadbv1alpha1.BackupStorage{S3: s3}, nil
}

// SyncBackup creates or updates the mariadb-operator CR that backs up the
// Instance to the S3 storage referenced by backup.spec.storageRef — a Backup
// (logical) or PhysicalBackup (physical) depending on spec.parameters.type —
// then maps the operator's Complete condition into the BackupExecutionStatus
// the runtime reflects onto the Backup CR.
func (p *MariaDBProvider) SyncBackup(
	c *controller.Context,
	backup *backupv1alpha1.Backup,
) (controller.BackupExecutionStatus, error) {
	// Scheduled runs are mirrored from operator-produced Jobs; report the Job's
	// status without creating a new operator Backup.
	if backup.Spec.ScheduleName != "" {
		return syncScheduledRun(c, backup)
	}

	params, err := parseBackupParams(rawParameters(backup.Spec.Parameters))
	if err != nil {
		return controller.BackupExecutionStatus{}, err
	}
	if isPhysical(params) {
		return syncPhysicalBackup(c, backup, params)
	}

	mdb := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(mdb, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.BackupExecutionStatus{
				State:   backupv1alpha1.BackupStatePending,
				Message: "Waiting for MariaDB cluster to exist",
			}, nil
		}
		return controller.BackupExecutionStatus{}, fmt.Errorf("get MariaDB: %w", err)
	}

	opBackup := &mariadbv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backup.Name,
			Namespace: backup.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(c.Context(), c.Client(), opBackup, func() error {
		// The operator Backup spec is immutable (spec.storage, spec.mariaDbRef)
		// and backups are run-once, so only populate it on creation (empty
		// ResourceVersion means the object does not exist yet).
		if opBackup.ResourceVersion == "" {
			storage, err := buildOperatorS3Storage(c, backup.Spec.StorageRef.Name)
			if err != nil {
				return err
			}
			opBackup.Spec.MariaDBRef = mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: c.Name()},
				WaitForIt:       true,
			}
			opBackup.Spec.Storage = storage
			applyLogicalBackupParameters(&opBackup.Spec, params)
		}
		return controllerutil.SetControllerReference(backup, opBackup, c.Client().Scheme())
	}); err != nil {
		return controller.BackupExecutionStatus{}, fmt.Errorf("create or update MariaDB backup: %w", err)
	}

	exec := mapBackupState(opBackup.Status.Conditions)
	exec.OperatorBackupRef = &apicommon.TypedObjectRef{
		Group: mariadbv1alpha1.GroupVersion.Group,
		Kind:  "Backup",
		Name:  opBackup.Name,
	}
	return exec, nil
}

// applyLogicalBackupParameters applies the decoded parameters onto the operator
// (logical) Backup spec.
func applyLogicalBackupParameters(spec *mariadbv1alpha1.BackupSpec, params mariadbbackup.MariadbBackupParameters) {
	// A dump that includes mysql.global_priv only rewrites the table on restore;
	// without a privilege reload the server keeps serving the old in-memory
	// accounts, and the restored users cannot log in until a restart.
	spec.Args = []string{"--flush-privileges"}
	spec.Databases = params.Databases
	spec.IgnoreGlobalPriv = params.IgnoreGlobalPriv
	if params.Compression != "" {
		spec.Compression = mariadbv1alpha1.CompressAlgorithm(params.Compression)
	}
}

// SyncRestore creates or updates a mariadb-operator Restore CR that restores
// the Instance's MariaDB from the operator Backup mirrored by the Backup CR
// named in restore.spec.dataSource.backup.backupRef.
func (p *MariaDBProvider) SyncRestore(
	c *controller.Context,
	restore *backupv1alpha1.Restore,
) (controller.RestoreExecutionStatus, error) {
	if restore.Spec.DataSource.Type != backupv1alpha1.DataSourceTypeBackup {
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStateFailed,
			Message: fmt.Sprintf("Unsupported dataSource type %q", restore.Spec.DataSource.Type),
		}, nil
	}
	ref := restore.Spec.DataSource.Backup
	if ref == nil || ref.BackupRef.Name == "" {
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStateFailed,
			Message: "Restore dataSource.backup.backupRef.name is required",
		}, nil
	}

	sourceBackup := &backupv1alpha1.Backup{}
	if err := c.Get(sourceBackup, ref.BackupRef.Name); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.RestoreExecutionStatus{
				State:   backupv1alpha1.RestoreStatePending,
				Message: "Waiting for source Backup",
			}, nil
		}
		return controller.RestoreExecutionStatus{}, fmt.Errorf("get source Backup: %w", err)
	}

	// Physical backups can only be restored by seeding a brand-new Instance via
	// spec.dataSource (which sets MariaDB.spec.bootstrapFrom); the engine cannot
	// restore them in place into an existing Instance.
	sourceParams, err := parseBackupParams(rawParameters(sourceBackup.Spec.Parameters))
	if err != nil {
		return controller.RestoreExecutionStatus{}, err
	}
	if isPhysical(sourceParams) {
		return controller.RestoreExecutionStatus{
			State: backupv1alpha1.RestoreStateFailed,
			Message: "In-place restore of a physical backup is not supported; " +
				"seed a new Instance from this Backup via spec.dataSource instead",
		}, nil
	}
	// A logical dump carries its Instance's accounts (mysql.global_priv), which
	// would replace the target's while its credential Secrets stay unchanged,
	// locking the operator and the probes out of the target.
	if origin := sourceBackup.Spec.Origin.InstanceRef; origin != nil && origin.Name != restore.Spec.InstanceRef.Name {
		return controller.RestoreExecutionStatus{
			State: backupv1alpha1.RestoreStateFailed,
			Message: fmt.Sprintf("In-place restore of a backup taken from another Instance (%q) is not supported; "+
				"seed a new Instance from this Backup via spec.dataSource instead", origin.Name),
		}, nil
	}
	if sourceBackup.Status.State == backupv1alpha1.BackupStateFailed {
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStateFailed,
			Message: "Source Backup failed; cannot restore",
		}, nil
	}
	if sourceBackup.Status.State != backupv1alpha1.BackupStateSucceeded {
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStatePending,
			Message: "Waiting for source Backup to succeed",
		}, nil
	}

	// SyncBackup names the operator Backup after the Backup CR, so the same
	// name resolves the source; prefer the recorded ref when present.
	operatorBackupName := sourceBackup.Name
	if sourceBackup.Status.OperatorBackupRef != nil && sourceBackup.Status.OperatorBackupRef.Name != "" {
		operatorBackupName = sourceBackup.Status.OperatorBackupRef.Name
	}

	opRestore := &mariadbv1alpha1.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      restore.Name,
			Namespace: restore.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(c.Context(), c.Client(), opRestore, func() error {
		// The operator Restore spec is immutable and restores are run-once, so
		// only populate it on creation (empty ResourceVersion means the object
		// does not exist yet).
		if opRestore.ResourceVersion == "" {
			opRestore.Spec.MariaDBRef = mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: c.Name()},
				WaitForIt:       true,
			}
			opRestore.Spec.BackupRef = &mariadbv1alpha1.LocalObjectReference{Name: operatorBackupName}
		}
		return controllerutil.SetControllerReference(restore, opRestore, c.Client().Scheme())
	}); err != nil {
		return controller.RestoreExecutionStatus{}, fmt.Errorf("create or update MariaDB restore: %w", err)
	}

	out := mapRestoreState(opRestore.Status.Conditions)
	out.OperatorRestoreRef = &apicommon.TypedObjectRef{
		Group: mariadbv1alpha1.GroupVersion.Group,
		Kind:  "Restore",
		Name:  opRestore.Name,
	}
	return out, nil
}

// CleanupBackup deletes the operator Backup CR. The mariadb-operator does not
// purge S3 objects on Backup deletion (retention is time-based via
// maxRetention), so DeletionPolicy=Delete removes only the CR; the underlying
// data is left to the storage's retention policy.
func (p *MariaDBProvider) CleanupBackup(c *controller.Context, backup *backupv1alpha1.Backup) (bool, error) {
	// Scheduled runs own no operator resources: the run Job is owned by the
	// operator CronJob and its data is governed by the schedule's retention.
	if backup.Spec.ScheduleName != "" {
		return true, nil
	}
	params, err := parseBackupParams(rawParameters(backup.Spec.Parameters))
	if err != nil {
		return false, err
	}
	if isPhysical(params) {
		return cleanupPhysicalBackup(c, backup)
	}
	opBackup := &mariadbv1alpha1.Backup{}
	err = c.Get(opBackup, backup.Name)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get MariaDB backup for cleanup: %w", err)
	}
	if opBackup.DeletionTimestamp.IsZero() {
		if err := c.Delete(opBackup); err != nil {
			return false, fmt.Errorf("delete MariaDB backup: %w", err)
		}
	}
	return false, nil
}

// CleanupRestore deletes the operator Restore CR. The Restore is
// run-to-completion and carries no protective finalizer, so a single delete is
// sufficient.
func (p *MariaDBProvider) CleanupRestore(c *controller.Context, restore *backupv1alpha1.Restore) (bool, error) {
	opRestore := &mariadbv1alpha1.Restore{}
	err := c.Get(opRestore, restore.Name)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get MariaDB restore for cleanup: %w", err)
	}
	if opRestore.DeletionTimestamp.IsZero() {
		if err := c.Delete(opRestore); err != nil {
			return false, fmt.Errorf("delete MariaDB restore: %w", err)
		}
	}
	return false, nil
}

// mapBackupState translates the operator Backup's Complete condition into a
// BackupExecutionStatus. The operator reports success and terminal failure both
// as Complete=True, distinguished only by the condition reason, so the mapping
// keys on the reason rather than the status.
func mapBackupState(conditions []metav1.Condition) controller.BackupExecutionStatus {
	cond := meta.FindStatusCondition(conditions, mariadbv1alpha1.ConditionTypeComplete)
	if cond == nil {
		return controller.BackupExecutionStatus{State: backupv1alpha1.BackupStatePending}
	}
	switch cond.Reason {
	case mariadbv1alpha1.ConditionReasonJobComplete, mariadbv1alpha1.ConditionReasonCronJobSuccess:
		completedAt := cond.LastTransitionTime
		return controller.BackupExecutionStatus{
			State:       backupv1alpha1.BackupStateSucceeded,
			CompletedAt: &completedAt,
		}
	case mariadbv1alpha1.ConditionReasonJobFailed, mariadbv1alpha1.ConditionReasonFailed:
		return controller.BackupExecutionStatus{
			State:   backupv1alpha1.BackupStateFailed,
			Message: cond.Message,
		}
	default:
		return controller.BackupExecutionStatus{
			State:   backupv1alpha1.BackupStateRunning,
			Message: cond.Message,
		}
	}
}

// mapRestoreState translates the operator Restore's Complete condition into a
// RestoreExecutionStatus, keying on the reason for the same reason as
// mapBackupState.
func mapRestoreState(conditions []metav1.Condition) controller.RestoreExecutionStatus {
	cond := meta.FindStatusCondition(conditions, mariadbv1alpha1.ConditionTypeComplete)
	if cond == nil {
		return controller.RestoreExecutionStatus{State: backupv1alpha1.RestoreStatePending}
	}
	switch cond.Reason {
	case mariadbv1alpha1.ConditionReasonJobComplete, mariadbv1alpha1.ConditionReasonCronJobSuccess:
		completedAt := cond.LastTransitionTime
		return controller.RestoreExecutionStatus{
			State:       backupv1alpha1.RestoreStateSucceeded,
			CompletedAt: &completedAt,
		}
	case mariadbv1alpha1.ConditionReasonJobFailed, mariadbv1alpha1.ConditionReasonFailed:
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStateFailed,
			Message: cond.Message,
		}
	default:
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStateRunning,
			Message: cond.Message,
		}
	}
}
