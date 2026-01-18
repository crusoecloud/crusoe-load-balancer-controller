package utils

import "testing"

func TestGenerateServiceHash(t *testing.T) {
	tests := []struct {
		name       string
		serviceUID string
	}{
		{
			name:       "standard UUID",
			serviceUID: "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		},
		{
			name:       "different UUID",
			serviceUID: "11223344-5566-7788-99aa-bbccddeeff00",
		},
		{
			name:       "short UID",
			serviceUID: "test-uid-123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GenerateServiceHash(tt.serviceUID)

			// Test hash length
			if len(got) != 8 {
				t.Errorf("GenerateServiceHash() hash length = %d, want 8", len(got))
			}

			// Test determinism - same inputs produce same hash
			got2 := GenerateServiceHash(tt.serviceUID)
			if got != got2 {
				t.Errorf("GenerateServiceHash() not deterministic: got %s and %s", got, got2)
			}
		})
	}
}

func TestGenerateServiceHashUniqueness(t *testing.T) {
	// Test that different service UIDs produce different hashes
	hash1 := GenerateServiceHash("a1b2c3d4-e5f6-7890-abcd-ef1234567890")
	hash2 := GenerateServiceHash("11223344-5566-7788-99aa-bbccddeeff00")
	hash3 := GenerateServiceHash("22334455-6677-8899-aabb-ccddeeff0011")

	if hash1 == hash2 {
		t.Errorf("Different service UIDs should produce different hashes: got %s for both", hash1)
	}

	if hash1 == hash3 {
		t.Errorf("Different service UIDs should produce different hashes: got %s for both", hash1)
	}

	if hash2 == hash3 {
		t.Errorf("Different service UIDs should produce different hashes: got %s for both", hash2)
	}
}

func TestGenerateServiceHashFormat(t *testing.T) {
	hash := GenerateServiceHash("test-service-uid-12345")

	// Hash should be 8 hex characters (0-9, a-f)
	for _, ch := range hash {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			t.Errorf("Hash contains invalid hex character: %c in %s", ch, hash)
		}
	}
}
