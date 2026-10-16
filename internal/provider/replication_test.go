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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

func TestIsHATopology(t *testing.T) {
	tests := []struct {
		topology string
		want     bool
	}{
		{topology: "", want: false},
		{topology: "standalone", want: false},
		{topology: "galera", want: true},
		{topology: "replication", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.topology, func(t *testing.T) {
			if got := isHATopology(newTopologyContext(t, tt.topology, nil)); got != tt.want {
				t.Errorf("isHATopology(%q) = %v, want %v", tt.topology, got, tt.want)
			}
		})
	}
}

func TestValidateTopology_ReplicationReplicas(t *testing.T) {
	tests := []struct {
		name     string
		replicas *int32
		wantErr  bool
	}{
		{name: "default (unset) is 3", replicas: nil, wantErr: false},
		{name: "two nodes ok", replicas: ptr.To(int32(2)), wantErr: false},
		{name: "even count ok", replicas: ptr.To(int32(4)), wantErr: false},
		{name: "single node rejected", replicas: ptr.To(int32(1)), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTopologyContext(t, "replication", tt.replicas)
			err := validateTopology(c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateTopology() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateTopology_ReplicationImmutable(t *testing.T) {
	replicationMariaDB := func() *mariadbv1alpha1.MariaDB {
		return &mariadbv1alpha1.MariaDB{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: mariadbv1alpha1.MariaDBSpec{
				Replicas:    3,
				Replication: &mariadbv1alpha1.Replication{Enabled: true},
			},
		}
	}
	tests := []struct {
		name     string
		topology string
		existing *mariadbv1alpha1.MariaDB
		wantErr  bool
	}{
		{name: "replication stays replication", topology: "replication", existing: replicationMariaDB(), wantErr: false},
		{name: "standalone -> replication rejected", topology: "replication", existing: existingMariaDB(false), wantErr: true},
		{name: "galera -> replication rejected", topology: "replication", existing: existingMariaDB(true), wantErr: true},
		{name: "replication -> galera rejected", topology: "galera", existing: replicationMariaDB(), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replicas := ptr.To(int32(3))
			var c *controller.Context
			if tt.existing != nil {
				c = newTopologyContext(t, tt.topology, replicas, tt.existing)
			} else {
				c = newTopologyContext(t, tt.topology, replicas)
			}
			err := validateTopology(c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateTopology() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
