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
	"testing"
	"time"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	mariadbbackup "github.com/openeverest/provider-mariadb/definition/backupclasses/mariadb"
)

func physicalBackup(name string) *backupv1alpha1.Backup {
	return &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: backupv1alpha1.BackupSpec{
			Origin:     backupv1alpha1.BackupOrigin{
				Type:        backupv1alpha1.BackupOriginTypeInstance,
				InstanceRef: &commonv1alpha1.ObjectRef{Name: "db"},
			},
			StorageRef: commonv1alpha1.ObjectRef{Name: "s3"},
			ClassRef:   commonv1alpha1.ObjectRef{Name: "mariadb"},
			Parameters: &runtime.RawExtension{Raw: []byte(`{"type":"physical"}`)},
		},
	}
}

func TestApplyPhysicalBackupParametersDefaultsTarget(t *testing.T) {
	// Standalone instances have no replica, so the backup must target the
	// primary (PreferReplica) rather than the operator's Replica default.
	var spec mariadbv1alpha1.PhysicalBackupSpec
	applyPhysicalBackupParameters(&spec, mariadbbackup.MariadbBackupParameters{})
	require.NotNil(t, spec.Target)
	assert.Equal(t, mariadbv1alpha1.PhysicalBackupTargetPreferReplica, *spec.Target)
}

func TestSyncBackupDispatchesToPhysical(t *testing.T) {
	mdb := &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"}}
	sdkBackup := physicalBackup("backup-1")
	sdkBackup.Spec.Parameters = &runtime.RawExtension{
		Raw: []byte(`{"type":"physical","compression":"bzip2","target":"Replica"}`),
	}
	c := newContextWith(t, mdb, s3BackupStorage("s3", "https://minio.example.com:9000"), sdkBackup)

	p := &MariaDBProvider{}
	exec, err := p.SyncBackup(c, sdkBackup)
	require.NoError(t, err)
	require.NotNil(t, exec.OperatorBackupRef)
	assert.Equal(t, "PhysicalBackup", exec.OperatorBackupRef.Kind)

	// A PhysicalBackup — not a logical Backup — must be created.
	assert.True(t, controller.IsNotFound(c.Get(&mariadbv1alpha1.Backup{}, "backup-1")))

	opBackup := &mariadbv1alpha1.PhysicalBackup{}
	require.NoError(t, c.Get(opBackup, "backup-1"))
	assert.Equal(t, "db", opBackup.Spec.MariaDBRef.Name)
	assert.True(t, opBackup.Spec.MariaDBRef.WaitForIt)
	require.NotNil(t, opBackup.Spec.Storage.S3)
	assert.Equal(t, "minio.example.com:9000", opBackup.Spec.Storage.S3.Endpoint)
	assert.Equal(t, mariadbv1alpha1.CompressAlgorithm("bzip2"), opBackup.Spec.Compression)
	require.NotNil(t, opBackup.Spec.Target)
	assert.Equal(t, mariadbv1alpha1.PhysicalBackupTargetReplica, *opBackup.Spec.Target)
	require.Len(t, opBackup.OwnerReferences, 1)
	assert.Equal(t, "backup-1", opBackup.OwnerReferences[0].Name)
}

func TestCleanupBackupDispatchesToPhysical(t *testing.T) {
	opBackup := &mariadbv1alpha1.PhysicalBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-1", Namespace: "ns"},
	}
	c := newContextWith(t, opBackup)
	p := &MariaDBProvider{}

	done, err := p.CleanupBackup(c, physicalBackup("backup-1"))
	require.NoError(t, err)
	assert.False(t, done)
	assert.True(t, controller.IsNotFound(c.Get(&mariadbv1alpha1.PhysicalBackup{}, "backup-1")))
}

func TestSyncRestoreRejectsPhysicalInPlace(t *testing.T) {
	mdb := &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"}}
	sourceBackup := physicalBackup("backup-1")
	sourceBackup.Status.State = backupv1alpha1.BackupStateSucceeded
	restore := &backupv1alpha1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: "restore-1", Namespace: "ns"},
		Spec: backupv1alpha1.RestoreSpec{
			InstanceRef: commonv1alpha1.ObjectRef{Name: "db"},
			DataSource: backupv1alpha1.DataSource{
				Type:   backupv1alpha1.DataSourceTypeBackup,
				Backup: &backupv1alpha1.DataSourceBackup{BackupRef: commonv1alpha1.ObjectRef{Name: "backup-1"}},
			},
		},
	}
	c := newContextWith(t, mdb, sourceBackup, restore)

	p := &MariaDBProvider{}
	out, err := p.SyncRestore(c, restore)
	require.NoError(t, err)
	assert.Equal(t, backupv1alpha1.RestoreStateFailed, out.State)
	assert.Contains(t, out.Message, "spec.dataSource")

	// No operator Restore must be created for a physical source.
	assert.True(t, controller.IsNotFound(c.Get(&mariadbv1alpha1.Restore{}, "restore-1")))
}

func instanceSeededFrom(backupName string) *corev1alpha1.Instance {
	return &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
		Spec: corev1alpha1.InstanceSpec{
			DataSource: &backupv1alpha1.DataSource{
				Type:   backupv1alpha1.DataSourceTypeBackup,
				Backup: &backupv1alpha1.DataSourceBackup{BackupRef: commonv1alpha1.ObjectRef{Name: backupName}},
			},
		},
	}
}

func TestResolveBootstrapFrom(t *testing.T) {
	completedAt := metav1.NewTime(time.Now().Truncate(time.Second))

	t.Run("no data source yields no bootstrap", func(t *testing.T) {
		c := newContextWith(t)
		got, _, err := resolveBootstrapFrom(c)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("waits for a source backup that has not succeeded", func(t *testing.T) {
		source := physicalBackup("backup-1")
		source.Spec.Origin.Type = backupv1alpha1.BackupOriginTypeInstance
		source.Spec.Origin.InstanceRef = &commonv1alpha1.ObjectRef{Name: "source-db"}
		source.Status.State = backupv1alpha1.BackupStateRunning
		c := newContextForInstance(t, instanceSeededFrom("backup-1"),
			source, s3BackupStorage("s3", "https://minio.example.com:9000"))

		_, _, err := resolveBootstrapFrom(c)
		require.Error(t, err)
		var waitErr *controller.WaitError
		assert.ErrorAs(t, err, &waitErr)
	})

	t.Run("succeeded logical source builds a logical bootstrapFrom pinned to the source", func(t *testing.T) {
		source := physicalBackup("backup-1")
		source.Spec.Parameters = &runtime.RawExtension{Raw: []byte(`{"type":"logical"}`)}
		source.Spec.Origin.InstanceRef = &commonv1alpha1.ObjectRef{Name: "source-db"}
		source.Status.State = backupv1alpha1.BackupStateSucceeded
		source.Status.CompletedAt = &completedAt
		c := newContextForInstance(t, instanceSeededFrom("backup-1"),
			source, s3BackupStorage("s3", "https://minio.example.com:9000"))

		got, sourceInstance, err := resolveBootstrapFrom(c)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.NotNil(t, got.S3)
		assert.Equal(t, "source-db", got.S3.Prefix)
		assert.Equal(t, "source-db", sourceInstance)
		assert.Equal(t, mariadbv1alpha1.BackupContentTypeLogical, got.BackupContentType)
		require.NotNil(t, got.TargetRecoveryTime)
		assert.Equal(t, completedAt.Time, got.TargetRecoveryTime.Time)
	})

	t.Run("source backup without a type defaults to logical", func(t *testing.T) {
		source := physicalBackup("backup-1")
		source.Spec.Parameters = nil
		source.Status.State = backupv1alpha1.BackupStateSucceeded
		c := newContextForInstance(t, instanceSeededFrom("backup-1"),
			source, s3BackupStorage("s3", "https://minio.example.com:9000"))

		got, _, err := resolveBootstrapFrom(c)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, mariadbv1alpha1.BackupContentTypeLogical, got.BackupContentType)
	})

	t.Run("failed source backup is a terminal seeding failure", func(t *testing.T) {
		source := physicalBackup("backup-1")
		source.Spec.Parameters = &runtime.RawExtension{Raw: []byte(`{"type":"logical"}`)}
		source.Status.State = backupv1alpha1.BackupStateFailed
		c := newContextForInstance(t, instanceSeededFrom("backup-1"),
			source, s3BackupStorage("s3", "https://minio.example.com:9000"))

		_, _, err := resolveBootstrapFrom(c)
		var dsErr *controller.DataSourceConfigError
		assert.ErrorAs(t, err, &dsErr)
	})

	t.Run("succeeded physical source builds bootstrapFrom pinned to the source", func(t *testing.T) {
		source := physicalBackup("backup-1")
		source.Spec.Origin.Type = backupv1alpha1.BackupOriginTypeInstance
		source.Spec.Origin.InstanceRef = &commonv1alpha1.ObjectRef{Name: "source-db"}
		source.Status.State = backupv1alpha1.BackupStateSucceeded
		source.Status.CompletedAt = &completedAt
		c := newContextForInstance(t, instanceSeededFrom("backup-1"),
			source, s3BackupStorage("s3", "https://minio.example.com:9000"))

		got, sourceInstance, err := resolveBootstrapFrom(c)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.NotNil(t, got.S3)
		assert.Equal(t, "source-db", got.S3.Prefix)
		assert.Equal(t, "source-db", sourceInstance)
		assert.Equal(t, mariadbv1alpha1.BackupContentTypePhysical, got.BackupContentType)
		require.NotNil(t, got.TargetRecoveryTime)
		assert.Equal(t, completedAt.Time, got.TargetRecoveryTime.Time)
	})
}

func credentialSecret(name, key, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Data:       map[string][]byte{key: []byte(value)},
	}
}

func TestEnsureRestoreCredentials(t *testing.T) {
	t.Run("copies source root and user secrets onto the target names", func(t *testing.T) {
		srcRoot := credentialSecret(rootSecretName("source-db"), rootPasswordSecretKey, "r00t")
		srcUser := credentialSecret(userSecretName("source-db"), userPasswordSecretKey, "s3cret")
		c := newContextWith(t, srcRoot, srcUser)

		require.NoError(t, ensureRestoreCredentials(c, "source-db"))

		gotRoot := &corev1.Secret{}
		require.NoError(t, c.Get(gotRoot, rootSecretName("db")))
		assert.Equal(t, []byte("r00t"), gotRoot.Data[rootPasswordSecretKey])

		gotUser := &corev1.Secret{}
		require.NoError(t, c.Get(gotUser, userSecretName("db")))
		assert.Equal(t, []byte("s3cret"), gotUser.Data[userPasswordSecretKey])
	})

	t.Run("leaves an existing target secret untouched", func(t *testing.T) {
		srcRoot := credentialSecret(rootSecretName("source-db"), rootPasswordSecretKey, "r00t")
		srcUser := credentialSecret(userSecretName("source-db"), userPasswordSecretKey, "s3cret")
		existing := credentialSecret(rootSecretName("db"), rootPasswordSecretKey, "keep-me")
		c := newContextWith(t, srcRoot, srcUser, existing)

		require.NoError(t, ensureRestoreCredentials(c, "source-db"))

		gotRoot := &corev1.Secret{}
		require.NoError(t, c.Get(gotRoot, rootSecretName("db")))
		assert.Equal(t, []byte("keep-me"), gotRoot.Data[rootPasswordSecretKey])
	})

	t.Run("missing source secret is a terminal seeding failure", func(t *testing.T) {
		c := newContextWith(t)

		err := ensureRestoreCredentials(c, "source-db")
		require.Error(t, err)
		var dsErr *controller.DataSourceConfigError
		assert.ErrorAs(t, err, &dsErr)
	})
}

func TestSyncDataSourceStatus(t *testing.T) {
	physicalBootstrapMariaDB := func(ready bool) *mariadbv1alpha1.MariaDB {
		mdb := &mariadbv1alpha1.MariaDB{
			ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
			Spec: mariadbv1alpha1.MariaDBSpec{
				BootstrapFrom: &mariadbv1alpha1.BootstrapFrom{
					BackupContentType: mariadbv1alpha1.BackupContentTypePhysical,
				},
			},
		}
		if ready {
			mdb.Status.Conditions = []metav1.Condition{{
				Type:               mariadbv1alpha1.ConditionTypeReady,
				Status:             metav1.ConditionTrue,
				Reason:             "Ready",
				LastTransitionTime: metav1.Now(),
			}}
		}
		return mdb
	}

	t.Run("ready cluster reports succeeded", func(t *testing.T) {
		c := newContextForInstance(t, instanceSeededFrom("backup-1"), physicalBootstrapMariaDB(true))
		require.NoError(t, syncDataSourceStatus(c))
		got := c.GetDataSourceStatus()
		require.NotNil(t, got)
		assert.True(t, got.Done)
		assert.Equal(t, controller.DataSourceStateSucceeded, got.State)
	})

	t.Run("unready cluster reports restoring", func(t *testing.T) {
		c := newContextForInstance(t, instanceSeededFrom("backup-1"), physicalBootstrapMariaDB(false))
		require.NoError(t, syncDataSourceStatus(c))
		got := c.GetDataSourceStatus()
		require.NotNil(t, got)
		assert.False(t, got.Done)
		assert.Equal(t, controller.DataSourceStateRestoring, got.State)
	})

	t.Run("no data source is a no-op", func(t *testing.T) {
		c := newContextWith(t, &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"}})
		require.NoError(t, syncDataSourceStatus(c))
		assert.Nil(t, c.GetDataSourceStatus())
	})
}

func logicalBootstrapMariaDB(conditions ...metav1.Condition) *mariadbv1alpha1.MariaDB {
	for i := range conditions {
		conditions[i].LastTransitionTime = metav1.Now()
	}
	return &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
		Spec: mariadbv1alpha1.MariaDBSpec{
			BootstrapFrom: &mariadbv1alpha1.BootstrapFrom{
				BackupContentType: mariadbv1alpha1.BackupContentTypeLogical,
			},
		},
		Status: mariadbv1alpha1.MariaDBStatus{Conditions: conditions},
	}
}

var (
	mariadbReady = metav1.Condition{
		Type: mariadbv1alpha1.ConditionTypeReady, Status: metav1.ConditionTrue, Reason: "Ready",
	}
	backupRestored = metav1.Condition{
		Type:   mariadbv1alpha1.ConditionTypeBackupRestored,
		Status: metav1.ConditionTrue,
		Reason: mariadbv1alpha1.ConditionReasonRestoreBackup,
	}
)

func TestSyncDataSourceStatusLogical(t *testing.T) {
	t.Run("ready cluster without a restored backup is still restoring", func(t *testing.T) {
		// The operator brings the cluster up before it restores a logical
		// backup, so Ready alone must not end the seeding.
		c := newContextForInstance(t, instanceSeededFrom("backup-1"), logicalBootstrapMariaDB(mariadbReady))
		require.NoError(t, syncDataSourceStatus(c))
		got := c.GetDataSourceStatus()
		require.NotNil(t, got)
		assert.False(t, got.Done)
		assert.Equal(t, controller.DataSourceStateRestoring, got.State)
	})

	t.Run("restored and ready cluster reports succeeded", func(t *testing.T) {
		c := newContextForInstance(t, instanceSeededFrom("backup-1"),
			logicalBootstrapMariaDB(mariadbReady, backupRestored))
		require.NoError(t, syncDataSourceStatus(c))
		got := c.GetDataSourceStatus()
		require.NotNil(t, got)
		assert.True(t, got.Done)
		assert.Equal(t, controller.DataSourceStateSucceeded, got.State)
	})

	t.Run("failed bootstrap restore reports failed", func(t *testing.T) {
		mdb := logicalBootstrapMariaDB()
		restore := &mariadbv1alpha1.Restore{
			ObjectMeta: metav1.ObjectMeta{Name: mdb.RestoreKey().Name, Namespace: "ns"},
			Status: mariadbv1alpha1.RestoreStatus{Conditions: []metav1.Condition{{
				Type:               mariadbv1alpha1.ConditionTypeComplete,
				Status:             metav1.ConditionFalse,
				Reason:             mariadbv1alpha1.ConditionReasonJobFailed,
				Message:            "job failed",
				LastTransitionTime: metav1.Now(),
			}}},
		}
		c := newContextForInstance(t, instanceSeededFrom("backup-1"), mdb, restore)
		require.NoError(t, syncDataSourceStatus(c))
		got := c.GetDataSourceStatus()
		require.NotNil(t, got)
		assert.True(t, got.Done)
		assert.Equal(t, controller.DataSourceStateFailed, got.State)
		assert.Contains(t, got.Message, "job failed")
	})
}

func TestStatusMariaDBHoldsPhaseWhileSeeding(t *testing.T) {
	t.Run("restoring while a logical backup is not yet applied", func(t *testing.T) {
		c := newContextForInstance(t, instanceSeededFrom("backup-1"), logicalBootstrapMariaDB(mariadbReady))
		require.NoError(t, syncDataSourceStatus(c))

		status, err := StatusMariaDB(c)
		require.NoError(t, err)
		assert.Equal(t, corev1alpha1.InstancePhaseRestoring, status.Phase)
	})

	t.Run("failed when seeding failed", func(t *testing.T) {
		c := newContextForInstance(t, instanceSeededFrom("backup-1"), logicalBootstrapMariaDB(mariadbReady))
		c.SetDataSourceStatus(controller.DataSourceStatus{
			Done:  true,
			State: controller.DataSourceStateFailed,
		})

		status, err := StatusMariaDB(c)
		require.NoError(t, err)
		assert.Equal(t, corev1alpha1.InstancePhaseFailed, status.Phase)
	})
}

func TestSyncScheduledBackupsDispatchesToPhysical(t *testing.T) {
	mdb := &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"}}
	instance := instanceWithSchedule(corev1alpha1.InstanceBackupSchedule{
		Name:       "daily",
		Enabled:    true,
		Cron:       "0 0 * * *",
		Parameters: &runtime.RawExtension{Raw: []byte(`{"type":"physical"}`)},
	}, "s3")
	c := newContextForInstance(t, instance, mdb, s3BackupStorage("s3", "https://minio.example.com:9000"))

	require.NoError(t, SyncScheduledBackups(c))

	name := scheduledBackupName("db", "daily")
	opBackup := &mariadbv1alpha1.PhysicalBackup{}
	require.NoError(t, c.Get(opBackup, name))
	require.NotNil(t, opBackup.Spec.Schedule)
	assert.Equal(t, "0 0 * * *", opBackup.Spec.Schedule.Cron)
	assert.False(t, opBackup.Spec.Schedule.Suspend)
	assert.Equal(t, backupTypePhysical, opBackup.Labels[typeLabel])
	assert.Equal(t, "daily", opBackup.Labels[scheduleLabel])
	require.NotNil(t, opBackup.Spec.InheritMetadata)
	assert.Equal(t, "daily", opBackup.Spec.InheritMetadata.Labels[scheduleLabel])
}
