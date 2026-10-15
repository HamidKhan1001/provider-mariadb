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

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/internal/common"
)

func replicationInstance(replicas int32, withStorage bool) *corev1alpha1.Instance {
	in := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
		Spec: corev1alpha1.InstanceSpec{
			Topology: &corev1alpha1.TopologySpec{Type: "replication"},
			Components: map[string]corev1alpha1.ComponentSpec{
				common.ComponentEngine: {Type: common.ComponentTypeMariaDB, Replicas: ptr.To(replicas)},
			},
		},
	}
	if withStorage {
		in.Spec.Backup = &corev1alpha1.InstanceBackupSpec{
			Enabled:  true,
			ClassRef: commonv1alpha1.ObjectRef{Name: "mariadb"},
			Storages: []corev1alpha1.InstanceBackupStorage{{StorageRef: commonv1alpha1.ObjectRef{Name: "s3"}}},
		}
	}
	return in
}

func replicationMariaDBWithReplicas(replicas int32) *mariadbv1alpha1.MariaDB {
	return &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
		Spec: mariadbv1alpha1.MariaDBSpec{
			Replicas:    replicas,
			Replication: &mariadbv1alpha1.Replication{Enabled: true},
		},
	}
}

func TestDesiredReplicaBootstrapFrom(t *testing.T) {
	t.Run("references the template on replication with a backup storage", func(t *testing.T) {
		got := desiredReplicaBootstrapFrom(newContextForInstance(t, replicationInstance(3, true)))
		require.NotNil(t, got)
		assert.Equal(t, replicaTemplateName("db"), got.PhysicalBackupTemplateRef.Name)
	})

	t.Run("unset without a backup storage", func(t *testing.T) {
		assert.Nil(t, desiredReplicaBootstrapFrom(newContextForInstance(t, replicationInstance(3, false))))
	})

	t.Run("unset on other topologies", func(t *testing.T) {
		in := replicationInstance(3, true)
		in.Spec.Topology.Type = "galera"
		assert.Nil(t, desiredReplicaBootstrapFrom(newContextForInstance(t, in)))
	})
}

func TestSyncReplicaTemplate(t *testing.T) {
	t.Run("creates a suspended template staging clones in the backup storage", func(t *testing.T) {
		c := newContextForInstance(t, replicationInstance(3, true),
			replicationMariaDBWithReplicas(3), s3BackupStorage("s3", "https://minio.example.com:9000"))
		require.NoError(t, SyncReplicaTemplate(c))

		tpl := &mariadbv1alpha1.PhysicalBackup{}
		require.NoError(t, c.Get(tpl, replicaTemplateName("db")))
		assert.Equal(t, "db", tpl.Spec.MariaDBRef.Name)
		assert.False(t, tpl.Spec.MariaDBRef.WaitForIt)
		require.NotNil(t, tpl.Spec.Schedule)
		assert.True(t, tpl.Spec.Schedule.Suspend)
		require.NotNil(t, tpl.Spec.Target)
		assert.Equal(t, mariadbv1alpha1.PhysicalBackupTargetPreferReplica, *tpl.Spec.Target)
		require.NotNil(t, tpl.Spec.Storage.S3)
		assert.Equal(t, "db-replica", tpl.Spec.Storage.S3.Prefix)
		require.Len(t, tpl.OwnerReferences, 1)
		assert.Equal(t, "MariaDB", tpl.OwnerReferences[0].Kind)
	})

	t.Run("removes the template when the backup storage is removed", func(t *testing.T) {
		existing := &mariadbv1alpha1.PhysicalBackup{
			ObjectMeta: metav1.ObjectMeta{Name: replicaTemplateName("db"), Namespace: "ns"},
		}
		c := newContextForInstance(t, replicationInstance(3, false), replicationMariaDBWithReplicas(3), existing)
		require.NoError(t, SyncReplicaTemplate(c))

		err := c.Get(&mariadbv1alpha1.PhysicalBackup{}, replicaTemplateName("db"))
		assert.True(t, controller.IsNotFound(err))
	})

	t.Run("waits for the MariaDB", func(t *testing.T) {
		c := newContextForInstance(t, replicationInstance(3, true), s3BackupStorage("s3", "https://minio.example.com:9000"))
		require.NoError(t, SyncReplicaTemplate(c))

		err := c.Get(&mariadbv1alpha1.PhysicalBackup{}, replicaTemplateName("db"))
		assert.True(t, controller.IsNotFound(err))
	})
}

func TestValidateReplicationScaleOut(t *testing.T) {
	tests := []struct {
		name     string
		instance *corev1alpha1.Instance
		existing *mariadbv1alpha1.MariaDB
		wantErr  bool
	}{
		{name: "scale out without a backup storage is rejected",
			instance: replicationInstance(4, false), existing: replicationMariaDBWithReplicas(3), wantErr: true},
		{name: "scale out with a backup storage is allowed",
			instance: replicationInstance(4, true), existing: replicationMariaDBWithReplicas(3)},
		{name: "unchanged replicas are allowed",
			instance: replicationInstance(3, false), existing: replicationMariaDBWithReplicas(3)},
		{name: "scale in is allowed",
			instance: replicationInstance(2, false), existing: replicationMariaDBWithReplicas(3)},
		{name: "creation is allowed",
			instance: replicationInstance(3, false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newContextForInstance(t, tt.instance)
			if tt.existing != nil {
				c = newContextForInstance(t, tt.instance, tt.existing)
			}
			err := validateReplicationScaleOut(c)
			assert.Equal(t, tt.wantErr, err != nil, "err = %v", err)
		})
	}
}
