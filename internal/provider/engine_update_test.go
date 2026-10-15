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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

	"github.com/openeverest/provider-mariadb/internal/common"
)

func TestImageMinorVersion(t *testing.T) {
	tests := []struct {
		image string
		want  [2]int
		ok    bool
	}{
		{image: "mariadb:12.3", want: [2]int{12, 3}, ok: true},
		{image: "mariadb:11.4.5-ubi9", want: [2]int{11, 4}, ok: true},
		{image: "registry:5000/library/mariadb:10.11@sha256:abc", want: [2]int{10, 11}, ok: true},
		{image: "registry:5000/library/mariadb", ok: false},
		{image: "mariadb@sha256:abc", ok: false},
		{image: "mariadb:lts", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			got, ok := imageMinorVersion(tt.image)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func engineUpdateInstance(image string, storage *corev1alpha1.Storage) *corev1alpha1.Instance {
	return &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
		Spec: corev1alpha1.InstanceSpec{
			Components: map[string]corev1alpha1.ComponentSpec{
				common.ComponentEngine: {Type: common.ComponentTypeMariaDB, Image: image, Storage: storage},
			},
		},
	}
}

func existingEngineMariaDB(image, size, class string) *mariadbv1alpha1.MariaDB {
	return &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
		Spec: mariadbv1alpha1.MariaDBSpec{
			Image: image,
			Storage: mariadbv1alpha1.Storage{
				Size:             ptr.To(resource.MustParse(size)),
				StorageClassName: class,
			},
		},
	}
}

func TestValidateEngineUpdate(t *testing.T) {
	sized := func(size string, class *string) *corev1alpha1.Storage {
		return &corev1alpha1.Storage{Size: resource.MustParse(size), StorageClass: class}
	}

	tests := []struct {
		name     string
		instance *corev1alpha1.Instance
		existing *mariadbv1alpha1.MariaDB
		wantErr  string
	}{
		{
			name:     "creation is not checked",
			instance: engineUpdateInstance("mariadb:10.11", sized("1Gi", nil)),
		},
		{
			name:     "minor downgrade is rejected",
			instance: engineUpdateInstance("mariadb:11.8", nil),
			existing: existingEngineMariaDB("mariadb:12.3", "10Gi", ""),
			wantErr:  "downgrading MariaDB from 12.3 to 11.8",
		},
		{
			name:     "upgrade is allowed",
			instance: engineUpdateInstance("mariadb:12.3", nil),
			existing: existingEngineMariaDB("mariadb:11.8", "10Gi", ""),
		},
		{
			name:     "patch-level change is allowed",
			instance: engineUpdateInstance("mariadb:11.4.4", nil),
			existing: existingEngineMariaDB("mariadb:11.4.5", "10Gi", ""),
		},
		{
			name:     "unparsable tags are allowed",
			instance: engineUpdateInstance("mariadb:lts", nil),
			existing: existingEngineMariaDB("mariadb:12.3", "10Gi", ""),
		},
		{
			name:     "storage shrink is rejected",
			instance: engineUpdateInstance("mariadb:12.3", sized("5Gi", nil)),
			existing: existingEngineMariaDB("mariadb:12.3", "10Gi", ""),
			wantErr:  "cannot be decreased from 10Gi to 5Gi",
		},
		{
			name:     "omitted size shrinking to the default is rejected",
			instance: engineUpdateInstance("mariadb:12.3", nil),
			existing: existingEngineMariaDB("mariadb:12.3", "20Gi", ""),
			wantErr:  "cannot be decreased from 20Gi to 10Gi",
		},
		{
			name:     "storage expansion is allowed",
			instance: engineUpdateInstance("mariadb:12.3", sized("20Gi", nil)),
			existing: existingEngineMariaDB("mariadb:12.3", "10Gi", ""),
		},
		{
			name:     "storage class change is rejected",
			instance: engineUpdateInstance("mariadb:12.3", sized("10Gi", ptr.To("fast"))),
			existing: existingEngineMariaDB("mariadb:12.3", "10Gi", "standard"),
			wantErr:  `cannot be changed on an existing instance (current: "standard")`,
		},
		{
			name:     "same storage class is allowed",
			instance: engineUpdateInstance("mariadb:12.3", sized("10Gi", ptr.To("standard"))),
			existing: existingEngineMariaDB("mariadb:12.3", "10Gi", "standard"),
		},
		{
			name:     "class on a default-class instance is left as is",
			instance: engineUpdateInstance("mariadb:12.3", sized("10Gi", ptr.To("fast"))),
			existing: existingEngineMariaDB("mariadb:12.3", "10Gi", ""),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newContextForInstance(t, tt.instance)
			if tt.existing != nil {
				c = newContextForInstance(t, tt.instance, tt.existing)
			}
			err := validateEngineUpdate(c)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func versionCatalogProvider() *corev1alpha1.Provider {
	return &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: common.ProviderName},
		Spec: corev1alpha1.ProviderSpec{
			ComponentTypes: map[string]corev1alpha1.ComponentType{
				common.ComponentTypeMariaDB: {
					DefaultVersion: "11.8",
					Versions: []corev1alpha1.ComponentVersion{
						{Version: "11.8", Image: "mariadb:11.8"},
						{Version: "12.3", Image: "mariadb:12.3"},
					},
				},
			},
			Components: map[string]corev1alpha1.Component{
				common.ComponentEngine: {Type: common.ComponentTypeMariaDB},
			},
			DefaultVersion: "11.8",
			Versions: []corev1alpha1.VersionBundle{
				{Name: "11.8", Components: map[string]string{common.ComponentEngine: "11.8"}},
				{Name: "12.3", Components: map[string]string{common.ComponentEngine: "12.3"}},
			},
		},
	}
}

// The runtime resolves version bundles before Sync but not before admission,
// so validation must resolve them itself.
func TestValidateEngineUpdateVersionBundles(t *testing.T) {
	tests := []struct {
		name          string
		version       string
		statusVersion string
		wantErr       bool
	}{
		{name: "downgrade via spec.version is rejected", version: "11.8", wantErr: true},
		{name: "staying on a bundle newer than the default is allowed", version: "12.3"},
		{name: "frozen status.version newer than the default is allowed", statusVersion: "12.3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := engineUpdateInstance("", nil)
			in.Spec.Version = tt.version
			in.Status.Version = tt.statusVersion
			c := newContextForInstance(t, in, versionCatalogProvider(),
				existingEngineMariaDB("mariadb:12.3", "10Gi", ""))
			err := validateEngineUpdate(c)
			assert.Equal(t, tt.wantErr, err != nil, "err = %v", err)
		})
	}
}
