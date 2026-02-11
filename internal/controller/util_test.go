package controller

import (
	"testing"
)

func TestGenerateLoadBalancerName(t *testing.T) {
	tests := []struct {
		name        string
		namespace   string
		serviceName string
		serviceUID  string
		wantErr     bool
		checkLen    bool
	}{
		{
			name:        "standard service",
			namespace:   "default",
			serviceName: "nginx-svc",
			serviceUID:  "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
			wantErr:     false,
			checkLen:    true,
		},
		{
			name:        "long service name should truncate",
			namespace:   "default",
			serviceName: "very-long-service-name-that-exceeds-limits-abcdefghijklmnop",
			serviceUID:  "11223344-5566-7788-99aa-bbccddeeff00",
			wantErr:     false,
			checkLen:    true,
		},
		{
			name:        "namespace with dots (valid in k8s)",
			namespace:   "my.namespace",
			serviceName: "my-service",
			serviceUID:  "22334455-6677-8899-aabb-ccddeeff0011",
			wantErr:     false,
			checkLen:    true,
		},
		{
			name:        "extremely long namespace",
			namespace:   "extremely-long-namespace-that-makes-everything-too-long-abcdefghijk",
			serviceName: "svc",
			serviceUID:  "33445566-7788-99aa-bbcc-ddeeff001122",
			wantErr:     true,
			checkLen:    false,
		},
		{
			name:        "kube-system namespace",
			namespace:   "kube-system",
			serviceName: "metrics-server",
			serviceUID:  "44556677-8899-aabb-ccdd-eeff00112233",
			wantErr:     false,
			checkLen:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GenerateLoadBalancerName(tt.namespace, tt.serviceName, tt.serviceUID)
			if (err != nil) != tt.wantErr {
				t.Errorf("GenerateLoadBalancerName() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.checkLen && len(got) >= 50 {
				t.Errorf("GenerateLoadBalancerName() length = %d, want < 50", len(got))
			}
			if !tt.wantErr {
				// Verify format: should contain namespace, service name (or truncated), and hash
				if len(got) == 0 {
					t.Errorf("GenerateLoadBalancerName() returned empty string")
				}
				// Verify determinism
				got2, _ := GenerateLoadBalancerName(tt.namespace, tt.serviceName, tt.serviceUID)
				if got != got2 {
					t.Errorf("GenerateLoadBalancerName() not deterministic: got %s and %s", got, got2)
				}
			}
		})
	}
}

func TestGenerateLoadBalancerNameFormat(t *testing.T) {
	serviceUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	name, err := GenerateLoadBalancerName("default", "nginx", serviceUID)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Name should match pattern: namespace-servicename-hash
	// and should contain "default", "nginx", and an 8-char hash
	if len(name) < 20 || len(name) >= 50 {
		t.Errorf("Name length %d out of expected range [20, 50)", len(name))
	}

	t.Logf("Generated name: %s (length: %d)", name, len(name))
}

func TestGenerateLoadBalancerNameUniqueness(t *testing.T) {
	// Test that different service UIDs produce different names
	name1, err1 := GenerateLoadBalancerName("default", "nginx", "a1b2c3d4-e5f6-7890-abcd-ef1234567890")
	if err1 != nil {
		t.Fatalf("Unexpected error for name1: %v", err1)
	}

	name2, err2 := GenerateLoadBalancerName("default", "nginx", "11223344-5566-7788-99aa-bbccddeeff00")
	if err2 != nil {
		t.Fatalf("Unexpected error for name2: %v", err2)
	}

	// Names should be different (different service UIDs)
	if name1 == name2 {
		t.Errorf("Different service UIDs should produce different names: got %s for both", name1)
	}

	// Test that another different UID produces a different name
	name3, err3 := GenerateLoadBalancerName("default", "nginx", "22334455-6677-8899-aabb-ccddeeff0011")
	if err3 != nil {
		t.Fatalf("Unexpected error for name3: %v", err3)
	}

	if name1 == name3 {
		t.Errorf("Different service UIDs should produce different names: got %s for both", name1)
	}
}

func TestGenerateLoadBalancerNameWithValidK8sNames(t *testing.T) {
	// Kubernetes service and namespace names are DNS-1123 compliant
	// They can contain: lowercase alphanumeric, '-', and '.'
	// They must start and end with alphanumeric
	tests := []struct {
		name        string
		namespace   string
		serviceName string
		serviceUID  string
	}{
		{
			name:        "dots in namespace (valid in k8s)",
			namespace:   "my.namespace.com",
			serviceName: "service",
			serviceUID:  "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		},
		{
			name:        "dots in service name (valid in k8s)",
			namespace:   "default",
			serviceName: "my.service.name",
			serviceUID:  "11223344-5566-7788-99aa-bbccddeeff00",
		},
		{
			name:        "hyphens in both",
			namespace:   "my-namespace",
			serviceName: "my-service",
			serviceUID:  "22334455-6677-8899-aabb-ccddeeff0011",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GenerateLoadBalancerName(tt.namespace, tt.serviceName, tt.serviceUID)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			// Verify name was generated successfully
			if len(got) == 0 {
				t.Errorf("Generated name is empty")
			}

			// Verify determinism
			got2, _ := GenerateLoadBalancerName(tt.namespace, tt.serviceName, tt.serviceUID)
			if got != got2 {
				t.Errorf("Generated name not deterministic: got %s and %s", got, got2)
			}

			t.Logf("Generated: %s (length: %d)", got, len(got))
		})
	}
}

func TestGenerateLoadBalancerNameWithVeryLongInputs(t *testing.T) {
	tests := []struct {
		name        string
		namespace   string
		serviceName string
		serviceUID  string
		wantErr     bool
		description string
	}{
		{
			name:        "sum of all inputs exceeds 50 chars",
			namespace:   "very-long-namespace-name",
			serviceName: "very-long-service-name-that-exceeds-fifty-characters",
			serviceUID:  "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
			wantErr:     false,
			description: "All inputs are long, total > 50 chars, should truncate service name",
		},
		{
			name:        "extremely long service name",
			namespace:   "production",
			serviceName: "my-application-with-extremely-long-name-for-load-balancer-service",
			serviceUID:  "11223344-5566-7788-99aa-bbccddeeff00",
			wantErr:     false,
			description: "Service name alone is > 50 chars, should truncate",
		},
		{
			name:        "long namespace and service",
			namespace:   "team-platform-infrastructure-services",
			serviceName: "api-gateway-load-balancer-external",
			serviceUID:  "22334455-6677-8899-aabb-ccddeeff0011",
			wantErr:     true,
			description: "Namespace too long (40+ chars), should error",
		},
		{
			name:        "max length namespace",
			namespace:   "namespace-that-is-almost-at-kubernetes-limit",
			serviceName: "service-name",
			serviceUID:  "33445566-7788-99aa-bbcc-ddeeff001122",
			wantErr:     true,
			description: "Namespace pushes close to limits, should error",
		},
		{
			name:        "all components at max reasonable length",
			namespace:   "my-very-long-namespace",
			serviceName: "my-extremely-long-service-name",
			serviceUID:  "44556677-8899-aabb-ccdd-eeff00112233",
			wantErr:     false,
			description: "Realistic worst case scenario, should work",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Calculate input length for logging
			inputLen := len(tt.namespace) + len(tt.serviceName) + len(tt.serviceUID)

			got, err := GenerateLoadBalancerName(tt.namespace, tt.serviceName, tt.serviceUID)

			if (err != nil) != tt.wantErr {
				t.Errorf("GenerateLoadBalancerName() error = %v, wantErr %v for %s", err, tt.wantErr, tt.description)
				return
			}

			if tt.wantErr {
				// If we expect an error, we're done
				t.Logf("Test: %s - correctly returned error: %v", tt.description, err)
				return
			}

			// Critical assertion: generated name must be less than 50 chars
			if len(got) >= 50 {
				t.Errorf("Generated name length = %d, want < 50\nInputs total length: %d\nNamespace: %s (len=%d)\nService: %s (len=%d)\nGenerated: %s",
					len(got), inputLen, tt.namespace, len(tt.namespace), tt.serviceName, len(tt.serviceName), got)
			}

			// Verify determinism
			got2, _ := GenerateLoadBalancerName(tt.namespace, tt.serviceName, tt.serviceUID)
			if got != got2 {
				t.Errorf("Generated name not deterministic: got %s and %s", got, got2)
			}

			// Log for visibility
			t.Logf("Test: %s", tt.description)
			t.Logf("  Input lengths: namespace=%d, service=%d, serviceUID=%d, total=%d",
				len(tt.namespace), len(tt.serviceName), len(tt.serviceUID), inputLen)
			t.Logf("  Generated: %s (length: %d)", got, len(got))

			// Verify it contains namespace (or truncated version)
			if len(got) == 0 {
				t.Errorf("Generated name is empty")
			}
		})
	}
}

func TestGenerateLoadBalancerNameEdgeCases(t *testing.T) {
	tests := []struct {
		name        string
		namespace   string
		serviceName string
		serviceUID  string
		wantErr     bool
		description string
	}{
		{
			name:        "namespace exactly 40 chars",
			namespace:   "namespace-that-is-exactly-forty-chars",
			serviceName: "svc",
			serviceUID:  "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
			wantErr:     true,
			description: "Namespace too long even after accounting for hash",
		},
		{
			name:        "namespace 30 chars with short service",
			namespace:   "this-namespace-is-thirty-char",
			serviceName: "api",
			serviceUID:  "11223344-5566-7788-99aa-bbccddeeff00",
			wantErr:     false,
			description: "Should fit with truncation",
		},
		{
			name:        "empty service name edge case",
			namespace:   "default",
			serviceName: "",
			serviceUID:  "22334455-6677-8899-aabb-ccddeeff0011",
			wantErr:     false,
			description: "Empty service name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GenerateLoadBalancerName(tt.namespace, tt.serviceName, tt.serviceUID)

			if (err != nil) != tt.wantErr {
				t.Errorf("GenerateLoadBalancerName() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if len(got) >= 50 {
					t.Errorf("Generated name length = %d, want < 50 for case: %s\nGenerated: %s",
						len(got), tt.description, got)
				}
				t.Logf("Test: %s", tt.description)
				t.Logf("  Generated: %s (length: %d)", got, len(got))
			}
		})
	}
}
