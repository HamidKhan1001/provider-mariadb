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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/internal/common"
)

func newContextForInstance(t *testing.T, instance *corev1alpha1.Instance, objs ...client.Object) *controller.Context {
	t.Helper()
	scheme := newBackupTestScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(append([]client.Object{instance}, objs...)...).
		Build()
	return controller.NewContext(context.Background(), fakeClient, instance, common.ProviderName)
}

func instanceWithSchedule(schedule corev1alpha1.InstanceBackupSchedule, storageName string) *corev1alpha1.Instance {
	return &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
		Spec: corev1alpha1.InstanceSpec{
			Backup: &corev1alpha1.InstanceBackupSpec{
				Enabled:  true,
				ClassRef: commonv1alpha1.ObjectRef{Name: "mariadb"},
				Storages: []corev1alpha1.InstanceBackupStorage{
					{
						StorageRef: commonv1alpha1.ObjectRef{Name: storageName},
						Schedules:  []corev1alpha1.InstanceBackupSchedule{schedule},
					},
				},
			},
		},
	}
}

func TestScheduledBackupName(t *testing.T) {
	name := scheduledBackupName("db", "daily")
	assert.Equal(t, scheduledBackupName("db", "daily"), name, "must be stable")
	assert.NotEqual(t, scheduledBackupName("db", "weekly"), name)
	assert.LessOrEqual(t, len(name), 52, "must fit the CronJob name limit")
	assert.Contains(t, name, "mdb-sched-")
}

func TestDeriveMaxRetention(t *testing.T) {
	t.Run("no retention keeps all", func(t *testing.T) {
		got, err := deriveMaxRetention("0 0 * * *", nil)
		require.NoError(t, err)
		assert.Equal(t, keepAllRetention, got.Duration)
	})

	t.Run("daily with 3 copies retains 4 days", func(t *testing.T) {
		got, err := deriveMaxRetention("0 0 * * *", countRetentionOf(3))
		require.NoError(t, err)
		assert.Equal(t, 4*24*time.Hour, got.Duration)
	})

	t.Run("invalid cron errors", func(t *testing.T) {
		_, err := deriveMaxRetention("not-a-cron", countRetentionOf(3))
		require.Error(t, err)
	})

	t.Run("time retention maps directly", func(t *testing.T) {
		for in, want := range map[string]time.Duration{
			"30d": 30 * 24 * time.Hour,
			"4w":  28 * 24 * time.Hour,
			"2m":  60 * 24 * time.Hour,
		} {
			got, err := deriveMaxRetention("0 0 * * *", &corev1alpha1.BackupScheduleRetention{
				Type:     corev1alpha1.BackupScheduleRetentionTypeTime,
				Duration: in,
			})
			require.NoError(t, err, in)
			assert.Equal(t, want, got.Duration, in)
		}
	})

	t.Run("invalid time retention errors", func(t *testing.T) {
		for _, in := range []string{"", "d", "0d", "10h", "xw"} {
			_, err := deriveMaxRetention("0 0 * * *", &corev1alpha1.BackupScheduleRetention{
				Type:     corev1alpha1.BackupScheduleRetentionTypeTime,
				Duration: in,
			})
			require.Error(t, err, in)
		}
	})
}

func countRetentionOf(n int32) *corev1alpha1.BackupScheduleRetention {
	return &corev1alpha1.BackupScheduleRetention{Type: corev1alpha1.BackupScheduleRetentionTypeCount, Count: &n}
}

func TestSyncScheduledBackupsCreatesOperatorBackup(t *testing.T) {
	instance := instanceWithSchedule(corev1alpha1.InstanceBackupSchedule{
		Name:      "daily",
		Enabled:   true,
		Cron:      "0 0 * * *",
		Retention: countRetentionOf(3),
	}, "s3")
	mdb := &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"}}
	c := newContextForInstance(t, instance, mdb, s3BackupStorage("s3", "https://minio.example.com:9000"))

	require.NoError(t, SyncScheduledBackups(c))

	opBackup := &mariadbv1alpha1.Backup{}
	require.NoError(t, c.Get(opBackup, scheduledBackupName("db", "daily")))
	require.NotNil(t, opBackup.Spec.Schedule)
	assert.Equal(t, "0 0 * * *", opBackup.Spec.Schedule.Cron)
	assert.False(t, opBackup.Spec.Schedule.Suspend)
	assert.Equal(t, []string{"--flush-privileges"}, opBackup.Spec.Args)
	assert.Equal(t, 4*24*time.Hour, opBackup.Spec.MaxRetention.Duration)
	assert.Equal(t, "daily", opBackup.Labels[scheduleLabel])
	require.NotNil(t, opBackup.Spec.InheritMetadata)
	assert.Equal(t, "daily", opBackup.Spec.InheritMetadata.Labels[scheduleLabel])
	assert.Equal(t, "s3", opBackup.Spec.InheritMetadata.Labels[storageLabel])
	assert.Equal(t, "mariadb", opBackup.Spec.InheritMetadata.Labels[classLabel])
	assert.Equal(t, backupTypeLogical, opBackup.Spec.InheritMetadata.Labels[typeLabel])
}

func TestSyncScheduledBackupsDisabledScheduleSuspends(t *testing.T) {
	instance := instanceWithSchedule(corev1alpha1.InstanceBackupSchedule{
		Name:    "daily",
		Enabled: false,
		Cron:    "0 0 * * *",
	}, "s3")
	mdb := &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"}}
	c := newContextForInstance(t, instance, mdb, s3BackupStorage("s3", "https://minio.example.com:9000"))

	require.NoError(t, SyncScheduledBackups(c))

	opBackup := &mariadbv1alpha1.Backup{}
	require.NoError(t, c.Get(opBackup, scheduledBackupName("db", "daily")))
	require.NotNil(t, opBackup.Spec.Schedule)
	assert.True(t, opBackup.Spec.Schedule.Suspend)
}

func TestSyncScheduledBackupsPrunesRemoved(t *testing.T) {
	// A stale operator Backup from a schedule no longer declared on the Instance.
	stale := &mariadbv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      scheduledBackupName("db", "old"),
			Namespace: "ns",
			Labels: map[string]string{
				instanceLabel: "db",
				scheduleLabel: "old",
			},
		},
	}
	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
		Spec: corev1alpha1.InstanceSpec{
			Backup: &corev1alpha1.InstanceBackupSpec{Enabled: true, ClassRef: commonv1alpha1.ObjectRef{Name: "mariadb"}},
		},
	}
	c := newContextForInstance(t, instance, stale)

	require.NoError(t, SyncScheduledBackups(c))

	err := c.Get(&mariadbv1alpha1.Backup{}, scheduledBackupName("db", "old"))
	assert.True(t, controller.IsNotFound(err))
}

func TestMirror(t *testing.T) {
	p := &MariaDBProvider{}

	t.Run("scheduled run Job is mirrored", func(t *testing.T) {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mdb-sched-abc-28900000",
				Namespace: "ns",
				Labels: map[string]string{
					scheduleLabel: "daily",
					instanceLabel: "db",
					storageLabel:  "s3",
					classLabel:    "mariadb",
					typeLabel:     backupTypePhysical,
				},
			},
		}
		got, err := p.Mirror(context.Background(), nil, job)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "mdb-sched-abc-28900000", got.Name)
		require.NotNil(t, got.Spec.Origin.InstanceRef)
		assert.Equal(t, "db", got.Spec.Origin.InstanceRef.Name)
		assert.Equal(t, "s3", got.Spec.StorageRef.Name)
		assert.Equal(t, "mariadb", got.Spec.ClassRef.Name)
		assert.Equal(t, "daily", got.Spec.ScheduleName)
		// The run's strategy is carried in parameters so restore can tell types apart.
		require.NotNil(t, got.Spec.Parameters)
		assert.JSONEq(t, `{"type":"physical"}`, string(got.Spec.Parameters.Raw))
	})

	t.Run("unlabeled Job is skipped", func(t *testing.T) {
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "ns"}}
		got, err := p.Mirror(context.Background(), nil, job)
		require.NoError(t, err)
		assert.Nil(t, got)
	})
}

func TestMapJobState(t *testing.T) {
	complete := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()},
	}}}
	assert.Equal(t, backupv1alpha1.BackupStateSucceeded, mapJobState(complete).State)
	require.NotNil(t, mapJobState(complete).CompletedAt)

	failed := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "boom"},
	}}}
	assert.Equal(t, backupv1alpha1.BackupStateFailed, mapJobState(failed).State)

	running := &batchv1.Job{}
	assert.Equal(t, backupv1alpha1.BackupStateRunning, mapJobState(running).State)
}

func TestSyncBackupScheduledRunReadsJob(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "mdb-sched-abc-28900000", Namespace: "ns"},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()},
		}},
	}
	mirrored := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "mdb-sched-abc-28900000", Namespace: "ns"},
		Spec: backupv1alpha1.BackupSpec{
			Origin: backupv1alpha1.BackupOrigin{
				Type:        backupv1alpha1.BackupOriginTypeInstance,
				InstanceRef: &commonv1alpha1.ObjectRef{Name: "db"},
			},
			ClassRef:     commonv1alpha1.ObjectRef{Name: "mariadb"},
			StorageRef:   commonv1alpha1.ObjectRef{Name: "s3"},
			ScheduleName: "daily",
		},
	}
	c := newContextForInstance(t, testInstance(), job, mirrored)

	p := &MariaDBProvider{}
	exec, err := p.SyncBackup(c, mirrored)
	require.NoError(t, err)
	assert.Equal(t, backupv1alpha1.BackupStateSucceeded, exec.State)
	require.NotNil(t, exec.OperatorBackupRef)
	assert.Equal(t, "Job", exec.OperatorBackupRef.Kind)

	// No new operator Backup is created for a scheduled run.
	err = c.Get(&mariadbv1alpha1.Backup{}, mirrored.Name)
	assert.True(t, controller.IsNotFound(err))
}

func TestCleanupBackupScheduledRunIsNoop(t *testing.T) {
	c := newContextForInstance(t, testInstance())
	p := &MariaDBProvider{}
	done, err := p.CleanupBackup(c, &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "mdb-sched-abc-1", Namespace: "ns"},
		Spec:       backupv1alpha1.BackupSpec{ScheduleName: "daily"},
	})
	require.NoError(t, err)
	assert.True(t, done)
}
