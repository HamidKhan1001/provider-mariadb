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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/definition"
	"github.com/openeverest/provider-mariadb/internal/common"
)

// newPITRContext builds a controller.Context bound to a caller-provided Instance
// (the shared newContextWith uses a fixed spec-less Instance).
func newPITRContext(t *testing.T, instance *corev1alpha1.Instance, objs ...client.Object) *controller.Context {
	t.Helper()
	scheme := newBackupTestScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(append([]client.Object{instance}, objs...)...).
		Build()
	return controller.NewContext(context.Background(), fakeClient, instance, common.ProviderName)
}

func physicalSchedule(name string) corev1alpha1.InstanceBackupSchedule {
	return corev1alpha1.InstanceBackupSchedule{
		Name:       name,
		Enabled:    true,
		Cron:       "0 0 * * *",
		Parameters: &runtime.RawExtension{Raw: []byte(`{"type":"physical"}`)},
	}
}

func logicalSchedule(name string) corev1alpha1.InstanceBackupSchedule {
	return corev1alpha1.InstanceBackupSchedule{
		Name:    name,
		Enabled: true,
		Cron:    "0 0 * * *",
	}
}

func pitrStorage(name string, enabled bool, schedules ...corev1alpha1.InstanceBackupSchedule) corev1alpha1.InstanceBackupStorage {
	s := corev1alpha1.InstanceBackupStorage{
		StorageRef: commonv1alpha1.ObjectRef{Name: name},
		Schedules:  schedules,
	}
	if enabled {
		s.PITR = &corev1alpha1.InstanceBackupStoragePITR{Enabled: true}
	}
	return s
}

func pitrInstance(topology string, storages ...corev1alpha1.InstanceBackupStorage) *corev1alpha1.Instance {
	in := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
		Spec: corev1alpha1.InstanceSpec{
			Backup: &corev1alpha1.InstanceBackupSpec{Enabled: true, Storages: storages},
		},
	}
	if topology != "" {
		in.Spec.Topology = &corev1alpha1.TopologySpec{Type: topology}
	}
	return in
}

func mariadbCR() *mariadbv1alpha1.MariaDB {
	return &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"}}
}

func TestPITREnabledStorage(t *testing.T) {
	t.Run("nil backup config", func(t *testing.T) {
		got, err := pitrEnabledStorage(nil)
		require.NoError(t, err)
		assert.Nil(t, got)
	})
	t.Run("no PITR-enabled storage", func(t *testing.T) {
		cfg := &corev1alpha1.InstanceBackupSpec{Enabled: true, Storages: []corev1alpha1.InstanceBackupStorage{
			pitrStorage("s1", false),
		}}
		got, err := pitrEnabledStorage(cfg)
		require.NoError(t, err)
		assert.Nil(t, got)
	})
	t.Run("single PITR-enabled storage wins", func(t *testing.T) {
		cfg := &corev1alpha1.InstanceBackupSpec{Enabled: true, Storages: []corev1alpha1.InstanceBackupStorage{
			pitrStorage("s1", false),
			pitrStorage("s2", true),
		}}
		got, err := pitrEnabledStorage(cfg)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "s2", got.StorageRef.Name)
	})
	t.Run("more than one PITR-enabled storage is rejected", func(t *testing.T) {
		cfg := &corev1alpha1.InstanceBackupSpec{Enabled: true, Storages: []corev1alpha1.InstanceBackupStorage{
			pitrStorage("s1", true),
			pitrStorage("s2", true),
		}}
		_, err := pitrEnabledStorage(cfg)
		require.Error(t, err)
		var cfgErr *controller.BackupConfigError
		assert.ErrorAs(t, err, &cfgErr)
	})
}

func TestFirstPhysicalSchedule(t *testing.T) {
	t.Run("returns the first physical schedule", func(t *testing.T) {
		s := pitrStorage("s1", true, logicalSchedule("l"), physicalSchedule("p"))
		got, err := firstPhysicalSchedule(&s)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "p", got.Name)
	})
	t.Run("no physical schedule is a config error", func(t *testing.T) {
		s := pitrStorage("s1", true, logicalSchedule("l"))
		_, err := firstPhysicalSchedule(&s)
		require.Error(t, err)
		var cfgErr *controller.BackupConfigError
		assert.ErrorAs(t, err, &cfgErr)
	})
}

func TestSyncPITRCreatesResources(t *testing.T) {
	in := pitrInstance(string(definition.TopologyTypeReplication),
		pitrStorage("s3", true, physicalSchedule("daily")))
	c := newPITRContext(t, in, mariadbCR(), s3BackupStorage("s3", "https://minio.example.com:9000"))

	require.NoError(t, SyncPITR(c))

	// PITR CR is created, owned by the Instance, referencing the base backup and
	// archiving binary logs under the dedicated prefix.
	pitr := &mariadbv1alpha1.PointInTimeRecovery{}
	require.NoError(t, c.Get(pitr, pitrCRName("db")))
	assert.Equal(t, pitrBaseBackupName("db"), pitr.Spec.PhysicalBackupRef.Name)
	require.NotNil(t, pitr.Spec.PointInTimeRecoveryStorage.S3)
	assert.Equal(t, "db-binlog", pitr.Spec.PointInTimeRecoveryStorage.S3.Prefix)
	require.Len(t, pitr.OwnerReferences, 1)
	assert.Equal(t, "db", pitr.OwnerReferences[0].Name)
	assert.Equal(t, "Instance", pitr.OwnerReferences[0].Kind)

	// Base PhysicalBackup is created, owned by the MariaDB, scheduled and immediate.
	base := &mariadbv1alpha1.PhysicalBackup{}
	require.NoError(t, c.Get(base, pitrBaseBackupName("db")))
	require.NotNil(t, base.Spec.Schedule)
	assert.Equal(t, "0 0 * * *", base.Spec.Schedule.Cron)
	require.NotNil(t, base.Spec.Schedule.Immediate)
	assert.True(t, *base.Spec.Schedule.Immediate)
	require.Len(t, base.OwnerReferences, 1)
	assert.Equal(t, "MariaDB", base.OwnerReferences[0].Kind)
	assert.Equal(t, "true", base.Labels[pitrBaseLabel])
}

func TestSyncPITRWaitsForMariaDB(t *testing.T) {
	in := pitrInstance(string(definition.TopologyTypeReplication),
		pitrStorage("s3", true, physicalSchedule("daily")))
	c := newPITRContext(t, in, s3BackupStorage("s3", "https://minio.example.com:9000"))

	err := SyncPITR(c)
	require.Error(t, err)
	var waitErr *controller.WaitError
	assert.ErrorAs(t, err, &waitErr)
}

func TestSyncPITRTeardownWhenDisabled(t *testing.T) {
	in := pitrInstance(string(definition.TopologyTypeReplication),
		pitrStorage("s3", false, physicalSchedule("daily")))
	existingPITR := &mariadbv1alpha1.PointInTimeRecovery{
		ObjectMeta: metav1.ObjectMeta{Name: pitrCRName("db"), Namespace: "ns"},
	}
	existingBase := &mariadbv1alpha1.PhysicalBackup{
		ObjectMeta: metav1.ObjectMeta{Name: pitrBaseBackupName("db"), Namespace: "ns"},
	}
	c := newPITRContext(t, in, existingPITR, existingBase)

	require.NoError(t, SyncPITR(c))
	assert.True(t, controller.IsNotFound(c.Get(&mariadbv1alpha1.PointInTimeRecovery{}, pitrCRName("db"))))
	assert.True(t, controller.IsNotFound(c.Get(&mariadbv1alpha1.PhysicalBackup{}, pitrBaseBackupName("db"))))
}

func TestDesiredPITRRef(t *testing.T) {
	t.Run("nil when PITR disabled", func(t *testing.T) {
		in := pitrInstance(string(definition.TopologyTypeReplication), pitrStorage("s3", false))
		c := newPITRContext(t, in)
		ref, err := desiredPITRRef(c)
		require.NoError(t, err)
		assert.Nil(t, ref)
	})
	t.Run("nil when the PITR CR does not exist yet", func(t *testing.T) {
		in := pitrInstance(string(definition.TopologyTypeReplication),
			pitrStorage("s3", true, physicalSchedule("daily")))
		c := newPITRContext(t, in)
		ref, err := desiredPITRRef(c)
		require.NoError(t, err)
		assert.Nil(t, ref)
	})
	t.Run("ref when the PITR CR exists", func(t *testing.T) {
		in := pitrInstance(string(definition.TopologyTypeReplication),
			pitrStorage("s3", true, physicalSchedule("daily")))
		pitr := &mariadbv1alpha1.PointInTimeRecovery{
			ObjectMeta: metav1.ObjectMeta{Name: pitrCRName("db"), Namespace: "ns"},
		}
		c := newPITRContext(t, in, pitr)
		ref, err := desiredPITRRef(c)
		require.NoError(t, err)
		require.NotNil(t, ref)
		assert.Equal(t, pitrCRName("db"), ref.Name)
	})
}

func TestValidatePITR(t *testing.T) {
	t.Run("passes for replication with a physical schedule", func(t *testing.T) {
		in := pitrInstance(string(definition.TopologyTypeReplication),
			pitrStorage("s3", true, physicalSchedule("daily")))
		c := newPITRContext(t, in)
		require.NoError(t, validatePITR(c))
	})
	t.Run("rejects non-replication topology", func(t *testing.T) {
		in := pitrInstance(string(definition.TopologyTypeStandalone),
			pitrStorage("s3", true, physicalSchedule("daily")))
		c := newPITRContext(t, in)
		err := validatePITR(c)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "replication")
	})
	t.Run("rejects missing physical schedule", func(t *testing.T) {
		in := pitrInstance(string(definition.TopologyTypeReplication),
			pitrStorage("s3", true, logicalSchedule("daily")))
		c := newPITRContext(t, in)
		err := validatePITR(c)
		require.Error(t, err)
	})
	t.Run("rejects more than one PITR storage", func(t *testing.T) {
		in := pitrInstance(string(definition.TopologyTypeReplication),
			pitrStorage("s1", true, physicalSchedule("daily")),
			pitrStorage("s2", true, physicalSchedule("daily")))
		c := newPITRContext(t, in)
		err := validatePITR(c)
		require.Error(t, err)
	})
	t.Run("no-op when PITR disabled", func(t *testing.T) {
		in := pitrInstance(string(definition.TopologyTypeStandalone), pitrStorage("s3", false))
		c := newPITRContext(t, in)
		require.NoError(t, validatePITR(c))
	})
}

func instanceSeededFromPITR(target backupv1alpha1.RecoveryTarget, date *metav1.Time) *corev1alpha1.Instance {
	return &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
		Spec: corev1alpha1.InstanceSpec{
			DataSource: &backupv1alpha1.DataSource{
				Type: backupv1alpha1.DataSourceTypePointInTime,
				PointInTime: &backupv1alpha1.DataSourcePointInTime{
					Source: backupv1alpha1.StreamSource{
						InstanceRef: &commonv1alpha1.ObjectRef{Name: "src"},
						StorageRef:  commonv1alpha1.ObjectRef{Name: "s3"},
					},
					RecoveryTarget: target,
					Date:           date,
				},
			},
		},
	}
}

func TestResolvePointInTimeBootstrapFrom(t *testing.T) {
	sourcePITR := &mariadbv1alpha1.PointInTimeRecovery{
		ObjectMeta: metav1.ObjectMeta{Name: pitrCRName("src"), Namespace: "ns"},
	}

	t.Run("date target sets the recovery time and physical content type", func(t *testing.T) {
		date := metav1.NewTime(time.Date(2026, 2, 20, 18, 0, 4, 0, time.UTC))
		in := instanceSeededFromPITR(backupv1alpha1.RecoveryTargetDate, &date)
		c := newPITRContext(t, in, sourcePITR)

		bootstrap, sourceInstance, err := resolveBootstrapFrom(c)
		require.NoError(t, err)
		require.NotNil(t, bootstrap)
		assert.Equal(t, "src", sourceInstance)
		require.NotNil(t, bootstrap.PointInTimeRecoveryRef)
		assert.Equal(t, pitrCRName("src"), bootstrap.PointInTimeRecoveryRef.Name)
		assert.Equal(t, mariadbv1alpha1.BackupContentTypePhysical, bootstrap.BackupContentType)
		require.NotNil(t, bootstrap.TargetRecoveryTime)
		assert.Equal(t, date.UTC(), bootstrap.TargetRecoveryTime.UTC())
	})

	t.Run("latest target leaves the recovery time unset", func(t *testing.T) {
		in := instanceSeededFromPITR(backupv1alpha1.RecoveryTargetLatest, nil)
		c := newPITRContext(t, in, sourcePITR)

		bootstrap, _, err := resolveBootstrapFrom(c)
		require.NoError(t, err)
		require.NotNil(t, bootstrap)
		assert.Nil(t, bootstrap.TargetRecoveryTime)
	})

	t.Run("waits for the source PITR CR", func(t *testing.T) {
		in := instanceSeededFromPITR(backupv1alpha1.RecoveryTargetLatest, nil)
		c := newPITRContext(t, in)

		_, _, err := resolveBootstrapFrom(c)
		require.Error(t, err)
		var waitErr *controller.WaitError
		assert.ErrorAs(t, err, &waitErr)
	})

	t.Run("missing source instanceRef is a data source error", func(t *testing.T) {
		in := instanceSeededFromPITR(backupv1alpha1.RecoveryTargetLatest, nil)
		in.Spec.DataSource.PointInTime.Source.InstanceRef = nil
		c := newPITRContext(t, in, sourcePITR)

		_, _, err := resolveBootstrapFrom(c)
		require.Error(t, err)
		var dsErr *controller.DataSourceConfigError
		assert.ErrorAs(t, err, &dsErr)
	})

	t.Run("date target without a date is a data source error", func(t *testing.T) {
		in := instanceSeededFromPITR(backupv1alpha1.RecoveryTargetDate, nil)
		c := newPITRContext(t, in, sourcePITR)

		_, _, err := resolveBootstrapFrom(c)
		require.Error(t, err)
		var dsErr *controller.DataSourceConfigError
		assert.ErrorAs(t, err, &dsErr)
	})
}

func TestPITRWindow(t *testing.T) {
	t.Run("unavailable without a recoverable time", func(t *testing.T) {
		got := pitrWindow(nil)
		require.NotNil(t, got)
		assert.Equal(t, corev1alpha1.PITRStateUnavailable, got.State)
		assert.Nil(t, got.LatestRestorableTime)
	})
	t.Run("available with a valid recoverable time", func(t *testing.T) {
		ts := "2026-02-20T18:00:04Z"
		got := pitrWindow(&ts)
		require.NotNil(t, got)
		assert.Equal(t, corev1alpha1.PITRStateAvailable, got.State)
		require.NotNil(t, got.LatestRestorableTime)
		assert.Equal(t, time.Date(2026, 2, 20, 18, 0, 4, 0, time.UTC), got.LatestRestorableTime.UTC())
	})
	t.Run("unavailable on an unparseable time", func(t *testing.T) {
		ts := "not-a-time"
		got := pitrWindow(&ts)
		require.NotNil(t, got)
		assert.Equal(t, corev1alpha1.PITRStateUnavailable, got.State)
	})
}

func TestBackupStorageStatuses(t *testing.T) {
	t.Run("reports the window for the PITR storage", func(t *testing.T) {
		in := pitrInstance(string(definition.TopologyTypeReplication),
			pitrStorage("s3", true, physicalSchedule("daily")))
		ts := "2026-02-20T18:00:04Z"
		pitr := &mariadbv1alpha1.PointInTimeRecovery{
			ObjectMeta: metav1.ObjectMeta{Name: pitrCRName("db"), Namespace: "ns"},
			Status:     mariadbv1alpha1.PointInTimeRecoveryStatus{LastRecoverableTime: ptr.To(ts)},
		}
		c := newPITRContext(t, in, pitr)

		p := &MariaDBProvider{}
		got, err := p.BackupStorageStatuses(c)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "s3", got[0].Name)
		require.NotNil(t, got[0].PITR)
		assert.Equal(t, corev1alpha1.PITRStateAvailable, got[0].PITR.State)
	})
	t.Run("nil when PITR is disabled", func(t *testing.T) {
		in := pitrInstance(string(definition.TopologyTypeReplication), pitrStorage("s3", false))
		c := newPITRContext(t, in)

		p := &MariaDBProvider{}
		got, err := p.BackupStorageStatuses(c)
		require.NoError(t, err)
		assert.Nil(t, got)
	})
}

// TestSyncRestoreRejectsPointInTimeInPlace locks in the seeding-only contract:
// point-in-time recovery can only seed a new Instance via spec.dataSource, so an
// in-place Restore CR of type PointInTime must be rejected without creating any
// operator Restore.
func TestSyncRestoreRejectsPointInTimeInPlace(t *testing.T) {
	restore := &backupv1alpha1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: "restore-pit", Namespace: "ns"},
		Spec: backupv1alpha1.RestoreSpec{
			InstanceRef: commonv1alpha1.ObjectRef{Name: "db"},
			DataSource: backupv1alpha1.DataSource{
				Type: backupv1alpha1.DataSourceTypePointInTime,
				PointInTime: &backupv1alpha1.DataSourcePointInTime{
					Source: backupv1alpha1.StreamSource{
						InstanceRef: &commonv1alpha1.ObjectRef{Name: "src"},
						StorageRef:  commonv1alpha1.ObjectRef{Name: "s3"},
					},
					RecoveryTarget: backupv1alpha1.RecoveryTargetLatest,
				},
			},
		},
	}
	c := newContextWith(t, restore)

	p := &MariaDBProvider{}
	out, err := p.SyncRestore(c, restore)
	require.NoError(t, err)
	assert.Equal(t, backupv1alpha1.RestoreStateFailed, out.State)
	assert.Contains(t, out.Message, "Unsupported dataSource type")

	// No operator Restore must be created for an in-place PITR attempt.
	assert.True(t, controller.IsNotFound(c.Get(&mariadbv1alpha1.Restore{}, "restore-pit")))
}

