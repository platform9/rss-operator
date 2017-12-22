// Copyright 2017 The etcd-operator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2eutil

import (
	api "github.com/beekhof/galera-operator/pkg/apis/galera/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func NewCluster(genName string, size int, labels map[string]string, annotations map[string]string) *api.GaleraCluster {
	return &api.GaleraCluster{
		TypeMeta: metav1.TypeMeta{
			Kind:       api.GaleraClusterResourceKind,
			APIVersion: api.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: genName,
			Labels:       labels,
			Annotations:  annotations,
		},
		Spec: api.ClusterSpec{
			Size: size,
			Pod: &api.PodPolicy{
				AntiAffinity: true,
				Labels:       map[string]string{"origin": "e2e-test"},
			},
			SequenceCommand:    []string{"/sequence.sh"},
			StartMemberCommand: []string{"/start.sh"},
			StopCommand:        []string{"/stop.sh"},
		},
	}
}

func ClusterWithVersion(cl *api.GaleraCluster, version string) *api.GaleraCluster {
	cl.Spec.Version = version
	return cl
}

// NameLabelSelector returns a label selector of the form name=<name>
func NameLabelSelector(name string) map[string]string {
	return map[string]string{"name": name}
}
