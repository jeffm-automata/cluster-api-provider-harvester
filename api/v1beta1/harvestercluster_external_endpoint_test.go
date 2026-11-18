/*
Copyright 2025 SUSE.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1beta1

import (
	"strings"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// With ipamType "external" no load balancer is created in Harvester, so the control plane
// endpoint can only come from the user and must be complete.
func TestValidateExternalControlPlaneEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		ipamType string
		endpoint clusterv1.APIEndpoint
		wantErr  string
	}{
		{"external with host and port", EXTERNAL, clusterv1.APIEndpoint{Host: "192.0.2.10", Port: 6443}, ""},
		{"external with a DNS name", EXTERNAL, clusterv1.APIEndpoint{Host: "api.cluster.example", Port: 6443}, ""},
		{"external without endpoint", EXTERNAL, clusterv1.APIEndpoint{}, "spec.controlPlaneEndpoint.host"},
		{"external without port", EXTERNAL, clusterv1.APIEndpoint{Host: "192.0.2.10"}, "spec.controlPlaneEndpoint.port"},
		{"external without host", EXTERNAL, clusterv1.APIEndpoint{Port: 6443}, "spec.controlPlaneEndpoint.host"},
		{"dhcp without endpoint, as before", DHCP, clusterv1.APIEndpoint{}, ""},
		{"pool without endpoint, as before", POOL, clusterv1.APIEndpoint{}, ""},
		{"unknown type", "static", clusterv1.APIEndpoint{Host: "192.0.2.10", Port: 6443}, "spec.loadBalancerConfig.ipamType must be"},
		{"empty type", "", clusterv1.APIEndpoint{}, "spec.loadBalancerConfig.ipamType must be"},
	}
	for _, tc := range cases {
		c := &HarvesterCluster{
			Spec: HarvesterClusterSpec{
				TargetNamespace:      "default",
				IdentitySecret:       SecretKey{Namespace: "default", Name: "id"},
				LoadBalancerConfig:   LoadBalancerConfig{IPAMType: IPAMType(tc.ipamType)},
				ControlPlaneEndpoint: tc.endpoint,
			},
		}

		_, err := validateHarvesterCluster(c)

		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error: %v", tc.name, err)
		case tc.wantErr != "" && err == nil:
			t.Errorf("%s: expected an error", tc.name)
		case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
			t.Errorf("%s: error should mention %q: %v", tc.name, tc.wantErr, err)
		}
	}
}
