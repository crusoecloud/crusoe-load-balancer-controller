/*
Copyright 2024.

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

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Service Controller", func() {
	Context("When reconciling a resource", func() {

		It("should successfully reconcile the resource", func() {

			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})

	Context("When generating load balancer names", func() {
		It("should generate unique LB names for services with different UIDs", func() {
			// Test that different service UIDs produce different LB names
			name1, err1 := GenerateLoadBalancerName("default", "nginx", "a1b2c3d4-e5f6-7890-abcd-ef1234567890")
			Expect(err1).NotTo(HaveOccurred())

			name2, err2 := GenerateLoadBalancerName("default", "nginx", "11223344-5566-7788-99aa-bbccddeeff00")
			Expect(err2).NotTo(HaveOccurred())

			// Names should be different (different service UIDs)
			Expect(name1).NotTo(Equal(name2))

			// But deterministic for same inputs
			name1Again, _ := GenerateLoadBalancerName("default", "nginx", "a1b2c3d4-e5f6-7890-abcd-ef1234567890")
			Expect(name1).To(Equal(name1Again))
		})

		It("should generate LB names within length limits", func() {
			tests := []struct {
				namespace   string
				serviceName string
				serviceUID  string
			}{
				{"default", "short", "a1b2c3d4-e5f6-7890-abcd-ef1234567890"},
				{"kube-system", "very-long-service-name-that-needs-truncation-abcdefghijklmnop", "11223344-5566-7788-99aa-bbccddeeff00"},
				{"my-namespace", "my-service", "22334455-6677-8899-aabb-ccddeeff0011"},
			}

			for _, tt := range tests {
				name, err := GenerateLoadBalancerName(tt.namespace, tt.serviceName, tt.serviceUID)
				Expect(err).NotTo(HaveOccurred())
				Expect(len(name)).To(BeNumerically("<", 50))
			}
		})

		It("should handle valid k8s names with dots and hyphens", func() {
			// Kubernetes allows dots and hyphens in service/namespace names (DNS-1123 compliant)
			name, err := GenerateLoadBalancerName("my.namespace", "my-service", "a1b2c3d4-e5f6-7890-abcd-ef1234567890")
			Expect(err).NotTo(HaveOccurred())
			Expect(len(name)).To(BeNumerically("<", 50))
			Expect(name).NotTo(BeEmpty())
		})
	})
})
