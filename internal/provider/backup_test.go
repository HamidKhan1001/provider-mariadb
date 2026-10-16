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
	"testing"
	"time"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/internal/common"
)

func newBackupTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, backupv1alpha1.AddToScheme(scheme))
	require.NoError(t, mariadbv1alpha1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	return scheme
}

func testInstance() *corev1alpha1.Instance {
	return &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
	}
}

func s3BackupStorage(name string, endpoint string) *backupv1alpha1.BackupStorage {
	return &backupv1alpha1.BackupStorage{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: backupv1alpha1.BackupStorageSpec{
			Type: backupv1alpha1.BackupStorageTypeS3,
			S3: &backupv1alpha1.BackupStorageS3Spec{
				Bucket:               "backups",
				Region:               "us-east-1",
				EndpointURL:          endpoint,
				CredentialsSecretRef: commonv1alpha1.SecretRef{Name: "creds"},
			},
		},
	}
}

func newContextWith(t *testing.T, objs ...client.Object) *controller.Context {
	t.Helper()
	scheme := newBackupTestScheme(t)
	instance := testInstance()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(append([]client.Object{instance}, objs...)...).
		Build()
	return controller.NewContext(context.Background(), fakeClient, instance, common.ProviderName)
}

func TestBuildOperatorS3Storage(t *testing.T) {
	t.Run("maps S3 storage and strips https scheme", func(t *testing.T) {
		c := newContextWith(t, s3BackupStorage("s3", "https://minio.example.com:9000"))

		got, err := buildOperatorS3Storage(c, "s3")
		require.NoError(t, err)
		require.NotNil(t, got.S3)
		assert.Equal(t, "backups", got.S3.Bucket)
		assert.Equal(t, "us-east-1", got.S3.Region)
		assert.Equal(t, "minio.example.com:9000", got.S3.Endpoint)
		assert.Equal(t, "db", got.S3.Prefix)
		require.NotNil(t, got.S3.AccessKeyIdSecretKeyRef)
		assert.Equal(t, "creds", got.S3.AccessKeyIdSecretKeyRef.Name)
		assert.Equal(t, s3AccessKeyIDSecretKey, got.S3.AccessKeyIdSecretKeyRef.Key)
		require.NotNil(t, got.S3.SecretAccessKeySecretKeyRef)
		assert.Equal(t, s3SecretAccessKeySecretKey, got.S3.SecretAccessKeySecretKeyRef.Key)
		require.NotNil(t, got.S3.TLS)
		assert.True(t, got.S3.TLS.Enabled)
	})

	t.Run("http endpoint disables TLS", func(t *testing.T) {
		c := newContextWith(t, s3BackupStorage("s3", "http://minio.example.com:9000"))

		got, err := buildOperatorS3Storage(c, "s3")
		require.NoError(t, err)
		assert.Equal(t, "minio.example.com:9000", got.S3.Endpoint)
		require.NotNil(t, got.S3.TLS)
		assert.False(t, got.S3.TLS.Enabled)
	})

	t.Run("missing storage returns a wait error", func(t *testing.T) {
		c := newContextWith(t)

		_, err := buildOperatorS3Storage(c, "absent")
		require.Error(t, err)
		var waitErr *controller.WaitError
		assert.ErrorAs(t, err, &waitErr)
	})

	t.Run("non-S3 storage is a config error", func(t *testing.T) {
		bs := &backupv1alpha1.BackupStorage{
			ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "ns"},
			Spec:       backupv1alpha1.BackupStorageSpec{Type: backupv1alpha1.BackupStorageTypeS3},
		}
		c := newContextWith(t, bs)

		_, err := buildOperatorS3Storage(c, "empty")
		require.Error(t, err)
		var cfgErr *controller.BackupConfigError
		assert.ErrorAs(t, err, &cfgErr)
	})
}

func TestParseBackupParams(t *testing.T) {
	t.Run("nil parameters yield zero value (logical)", func(t *testing.T) {
		got, err := parseBackupParams(nil)
		require.NoError(t, err)
		assert.Empty(t, got.Type)
		assert.False(t, isPhysical(got))
		assert.Empty(t, got.Compression)
		assert.Nil(t, got.Databases)
		assert.Nil(t, got.IgnoreGlobalPriv)
	})

	t.Run("decodes logical parameters", func(t *testing.T) {
		got, err := parseBackupParams([]byte(`{"compression":"gzip","databases":["app"],"ignoreGlobalPriv":true}`))
		require.NoError(t, err)
		assert.False(t, isPhysical(got))
		assert.Equal(t, "gzip", got.Compression)
		assert.Equal(t, []string{"app"}, got.Databases)
		require.NotNil(t, got.IgnoreGlobalPriv)
		assert.True(t, *got.IgnoreGlobalPriv)
	})

	t.Run("decodes physical type", func(t *testing.T) {
		got, err := parseBackupParams([]byte(`{"type":"physical","target":"Replica"}`))
		require.NoError(t, err)
		assert.True(t, isPhysical(got))
		assert.Equal(t, "Replica", got.Target)
	})

	t.Run("invalid JSON is a config error", func(t *testing.T) {
		_, err := parseBackupParams([]byte(`{`))
		require.Error(t, err)
		var cfgErr *controller.BackupConfigError
		assert.ErrorAs(t, err, &cfgErr)
	})
}

func completeCondition(reason string, status metav1.ConditionStatus, at time.Time) []metav1.Condition {
	return []metav1.Condition{{
		Type:               mariadbv1alpha1.ConditionTypeComplete,
		Status:             status,
		Reason:             reason,
		Message:            reason,
		LastTransitionTime: metav1.NewTime(at),
	}}
}

func TestMapBackupState(t *testing.T) {
	now := time.Now()

	t.Run("no condition is pending", func(t *testing.T) {
		got := mapBackupState(nil)
		assert.Equal(t, backupv1alpha1.BackupStatePending, got.State)
		assert.Nil(t, got.CompletedAt)
	})

	t.Run("job complete is succeeded with completion time", func(t *testing.T) {
		got := mapBackupState(completeCondition(
			mariadbv1alpha1.ConditionReasonJobComplete, metav1.ConditionTrue, now))
		assert.Equal(t, backupv1alpha1.BackupStateSucceeded, got.State)
		require.NotNil(t, got.CompletedAt)
		assert.WithinDuration(t, now, got.CompletedAt.Time, time.Second)
	})

	t.Run("job failed is failed", func(t *testing.T) {
		got := mapBackupState(completeCondition(
			mariadbv1alpha1.ConditionReasonJobFailed, metav1.ConditionTrue, now))
		assert.Equal(t, backupv1alpha1.BackupStateFailed, got.State)
	})

	t.Run("job running is running", func(t *testing.T) {
		got := mapBackupState(completeCondition(
			mariadbv1alpha1.ConditionReasonJobRunning, metav1.ConditionFalse, now))
		assert.Equal(t, backupv1alpha1.BackupStateRunning, got.State)
	})
}

func TestMapRestoreState(t *testing.T) {
	now := time.Now()

	assert.Equal(t, backupv1alpha1.RestoreStatePending, mapRestoreState(nil).State)
	assert.Equal(t, backupv1alpha1.RestoreStateSucceeded, mapRestoreState(completeCondition(
		mariadbv1alpha1.ConditionReasonJobComplete, metav1.ConditionTrue, now)).State)
	assert.Equal(t, backupv1alpha1.RestoreStateFailed, mapRestoreState(completeCondition(
		mariadbv1alpha1.ConditionReasonJobFailed, metav1.ConditionTrue, now)).State)
	assert.Equal(t, backupv1alpha1.RestoreStateRunning, mapRestoreState(completeCondition(
		mariadbv1alpha1.ConditionReasonJobRunning, metav1.ConditionFalse, now)).State)
}

func TestSyncBackup(t *testing.T) {
	mdb := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
	}
	sdkBackup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-1", Namespace: "ns"},
		Spec: backupv1alpha1.BackupSpec{
			Origin: backupv1alpha1.BackupOrigin{
				Type:        backupv1alpha1.BackupOriginTypeInstance,
				InstanceRef: &commonv1alpha1.ObjectRef{Name: "db"},
			},
			StorageRef: commonv1alpha1.ObjectRef{Name: "s3"},
			Parameters: &runtime.RawExtension{
				Raw: []byte(`{"compression":"gzip","databases":["app"],"ignoreGlobalPriv":true}`),
			},
		},
	}
	c := newContextWith(t, mdb, s3BackupStorage("s3", "https://minio.example.com:9000"), sdkBackup)

	p := &MariaDBProvider{}
	exec, err := p.SyncBackup(c, sdkBackup)
	require.NoError(t, err)
	assert.Equal(t, backupv1alpha1.BackupStatePending, exec.State)
	require.NotNil(t, exec.OperatorBackupRef)
	assert.Equal(t, "backup-1", exec.OperatorBackupRef.Name)
	assert.Equal(t, mariadbv1alpha1.GroupVersion.Group, exec.OperatorBackupRef.Group)

	opBackup := &mariadbv1alpha1.Backup{}
	require.NoError(t, c.Get(opBackup, "backup-1"))
	assert.Equal(t, "db", opBackup.Spec.MariaDBRef.Name)
	assert.True(t, opBackup.Spec.MariaDBRef.WaitForIt)
	require.NotNil(t, opBackup.Spec.Storage.S3)
	assert.Equal(t, "minio.example.com:9000", opBackup.Spec.Storage.S3.Endpoint)
	assert.Equal(t, mariadbv1alpha1.CompressAlgorithm("gzip"), opBackup.Spec.Compression)
	assert.Equal(t, []string{"app"}, opBackup.Spec.Databases)
	assert.Equal(t, []string{"--flush-privileges"}, opBackup.Spec.Args)
	require.NotNil(t, opBackup.Spec.IgnoreGlobalPriv)
	assert.True(t, *opBackup.Spec.IgnoreGlobalPriv)
	require.Len(t, opBackup.OwnerReferences, 1)
	assert.Equal(t, "backup-1", opBackup.OwnerReferences[0].Name)
}

func TestSyncBackupDoesNotMutateExistingSpec(t *testing.T) {
	mdb := &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"}}
	// An operator Backup created by an earlier reconcile (or older provider)
	// with a different storage endpoint. spec.storage is immutable, so a
	// re-sync must leave it untouched instead of attempting a rejected update.
	existing := &mariadbv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-1", Namespace: "ns"},
		Spec: mariadbv1alpha1.BackupSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: "db"},
				WaitForIt:       true,
			},
			Storage: mariadbv1alpha1.BackupStorage{
				S3: &mariadbv1alpha1.S3{Bucket: "backups", Endpoint: "old.example.com"},
			},
		},
	}
	sdkBackup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-1", Namespace: "ns"},
		Spec: backupv1alpha1.BackupSpec{
			Origin: backupv1alpha1.BackupOrigin{
				Type:        backupv1alpha1.BackupOriginTypeInstance,
				InstanceRef: &commonv1alpha1.ObjectRef{Name: "db"},
			},
			StorageRef: commonv1alpha1.ObjectRef{Name: "s3"},
		},
	}
	c := newContextWith(t, mdb, s3BackupStorage("s3", "https://new.example.com:9000"), sdkBackup, existing)

	p := &MariaDBProvider{}
	_, err := p.SyncBackup(c, sdkBackup)
	require.NoError(t, err)

	opBackup := &mariadbv1alpha1.Backup{}
	require.NoError(t, c.Get(opBackup, "backup-1"))
	require.NotNil(t, opBackup.Spec.Storage.S3)
	assert.Equal(t, "old.example.com", opBackup.Spec.Storage.S3.Endpoint)
}

func TestSyncBackupPendingWithoutMariaDB(t *testing.T) {
	sdkBackup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-1", Namespace: "ns"},
		Spec: backupv1alpha1.BackupSpec{
			Origin:     backupv1alpha1.BackupOrigin{
				Type:        backupv1alpha1.BackupOriginTypeInstance,
				InstanceRef: &commonv1alpha1.ObjectRef{Name: "db"},
			},
			StorageRef: commonv1alpha1.ObjectRef{Name: "s3"},
		},
	}
	c := newContextWith(t, s3BackupStorage("s3", "https://minio.example.com:9000"), sdkBackup)

	p := &MariaDBProvider{}
	exec, err := p.SyncBackup(c, sdkBackup)
	require.NoError(t, err)
	assert.Equal(t, backupv1alpha1.BackupStatePending, exec.State)
}

func TestSyncRestoreWaitsForSucceededBackup(t *testing.T) {
	mdb := &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"}}
	sourceBackup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-1", Namespace: "ns"},
		Status:     backupv1alpha1.BackupStatus{State: backupv1alpha1.BackupStateRunning},
	}
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
	assert.Equal(t, backupv1alpha1.RestoreStatePending, out.State)

	// The operator Restore must not be created until the source Backup succeeds.
	err = c.Get(&mariadbv1alpha1.Restore{}, "restore-1")
	assert.True(t, controller.IsNotFound(err))
}

func TestSyncRestoreCreatesOperatorRestore(t *testing.T) {
	mdb := &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"}}
	sourceBackup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-1", Namespace: "ns"},
		Status:     backupv1alpha1.BackupStatus{State: backupv1alpha1.BackupStateSucceeded},
	}
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
	require.NotNil(t, out.OperatorRestoreRef)
	assert.Equal(t, "restore-1", out.OperatorRestoreRef.Name)

	opRestore := &mariadbv1alpha1.Restore{}
	require.NoError(t, c.Get(opRestore, "restore-1"))
	assert.Equal(t, "db", opRestore.Spec.MariaDBRef.Name)
	require.NotNil(t, opRestore.Spec.BackupRef)
	assert.Equal(t, "backup-1", opRestore.Spec.BackupRef.Name)
}

func TestSyncRestoreRejectsBackupFromAnotherInstance(t *testing.T) {
	mdb := &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"}}
	sourceBackup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-1", Namespace: "ns"},
		Spec: backupv1alpha1.BackupSpec{
			Origin: backupv1alpha1.BackupOrigin{
				Type:        backupv1alpha1.BackupOriginTypeInstance,
				InstanceRef: &commonv1alpha1.ObjectRef{Name: "other-db"},
			},
		},
		Status: backupv1alpha1.BackupStatus{State: backupv1alpha1.BackupStateSucceeded},
	}
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

	err = c.Get(&mariadbv1alpha1.Restore{}, "restore-1")
	assert.True(t, controller.IsNotFound(err))
}

func TestCleanupBackup(t *testing.T) {
	t.Run("absent operator backup is done", func(t *testing.T) {
		c := newContextWith(t)
		p := &MariaDBProvider{}
		done, err := p.CleanupBackup(c, &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "backup-1", Namespace: "ns"},
		})
		require.NoError(t, err)
		assert.True(t, done)
	})

	t.Run("existing operator backup is deleted and not yet done", func(t *testing.T) {
		opBackup := &mariadbv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "backup-1", Namespace: "ns"},
		}
		c := newContextWith(t, opBackup)
		p := &MariaDBProvider{}
		done, err := p.CleanupBackup(c, &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "backup-1", Namespace: "ns"},
		})
		require.NoError(t, err)
		assert.False(t, done)
		assert.True(t, controller.IsNotFound(c.Get(&mariadbv1alpha1.Backup{}, "backup-1")))
	})
}
