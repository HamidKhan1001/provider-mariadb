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
	"reflect"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// replicaPrefixSuffix keeps the replica clones apart from the Instance's
// backups in the shared storage prefix.
const replicaPrefixSuffix = "-replica"

// replicaTemplateName names the PhysicalBackup the operator uses as a template
// to clone new replicas of a replication cluster.
func replicaTemplateName(instance string) string {
	return instance + "-replica-tpl"
}

// replicaTemplateStorage returns the backup storage used to stage replica
// clones: the first storage of an enabled spec.backup, or nil when the Instance
// has none, in which case the replication topology cannot scale out.
func replicaTemplateStorage(in *corev1alpha1.Instance) *corev1alpha1.InstanceBackupStorage {
	b := in.Spec.Backup
	if b == nil || !b.Enabled || len(b.Storages) == 0 {
		return nil
	}
	return &b.Storages[0]
}

// desiredReplicaBootstrapFrom points the operator at the replica template, so
// it can clone new replicas when scaling out a replication cluster.
func desiredReplicaBootstrapFrom(c *controller.Context) *mariadbv1alpha1.ReplicaBootstrapFrom {
	if !isReplicationTopology(c) || replicaTemplateStorage(c.Instance()) == nil {
		return nil
	}
	return &mariadbv1alpha1.ReplicaBootstrapFrom{
		PhysicalBackupTemplateRef: mariadbv1alpha1.LocalObjectReference{Name: replicaTemplateName(c.Name())},
	}
}

// SyncReplicaTemplate reconciles the PhysicalBackup template the operator uses
// to clone new replicas when scaling out a replication cluster. The template is
// never run on its own (its schedule is suspended); the operator creates a
// one-off PhysicalBackup from it, preferring a ready replica as the source.
func SyncReplicaTemplate(c *controller.Context) error {
	name := replicaTemplateName(c.Name())
	storage := replicaTemplateStorage(c.Instance())
	if !isReplicationTopology(c) || storage == nil {
		tpl := &mariadbv1alpha1.PhysicalBackup{}
		tpl.Name, tpl.Namespace = name, c.Namespace()
		if err := c.Delete(tpl); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete replica template: %w", err)
		}
		return nil
	}

	mdb := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(mdb, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get MariaDB: %w", err)
	}

	s3, err := buildOperatorS3(c, storage.StorageRef.Name, c.Name()+replicaPrefixSuffix)
	if err != nil {
		return err
	}
	desiredStorage := mariadbv1alpha1.PhysicalBackupStorage{S3: s3}

	// spec.storage is immutable on the operator PhysicalBackup; if it changed,
	// recreate the template so the new value takes effect.
	existing := &mariadbv1alpha1.PhysicalBackup{}
	if getErr := c.Get(existing, name); getErr == nil {
		if !reflect.DeepEqual(existing.Spec.Storage, desiredStorage) {
			if err := c.Delete(existing); err != nil {
				return fmt.Errorf("delete outdated replica template: %w", err)
			}
			return controller.WaitFor("Recreating replica template")
		}
	} else if !apierrors.IsNotFound(getErr) {
		return fmt.Errorf("get replica template: %w", getErr)
	}

	tpl := &mariadbv1alpha1.PhysicalBackup{ObjectMeta: c.ObjectMeta(name)}
	_, err = controllerutil.CreateOrUpdate(c.Context(), c.Client(), tpl, func() error {
		if tpl.ResourceVersion == "" {
			// The template must not wait for the MariaDB to be ready: it is
			// needed while the cluster is scaling out.
			tpl.Spec.MariaDBRef = mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: c.Name()},
			}
			tpl.Spec.Storage = desiredStorage
		}
		tpl.Spec.Schedule = &mariadbv1alpha1.PhysicalBackupSchedule{Suspend: true}
		tpl.Spec.Target = ptr.To(mariadbv1alpha1.PhysicalBackupTargetPreferReplica)
		return controllerutil.SetControllerReference(mdb, tpl, c.Client().Scheme())
	})
	if err != nil {
		return fmt.Errorf("reconcile replica template: %w", err)
	}
	return nil
}
