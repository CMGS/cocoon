package utils

import "testing"

func TestProcessNameMatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		actual   string
		expected string
		want     bool
	}{
		{
			name:     "exact match",
			actual:   "cloud-hypervisor",
			expected: "cloud-hypervisor",
			want:     true,
		},
		{
			name:     "linux comm truncated prefix",
			actual:   "cloud-hyperviso",
			expected: "cloud-hypervisor",
			want:     true,
		},
		{
			name:     "short prefix should not match",
			actual:   "cloud",
			expected: "cloud-hypervisor",
			want:     false,
		},
		{
			name:     "contains should not match",
			actual:   "hypervisor",
			expected: "cloud-hypervisor",
			want:     false,
		},
		{
			name:     "different binary",
			actual:   "qemu-system-x86_64",
			expected: "cloud-hypervisor",
			want:     false,
		},
		{
			name:     "empty expected",
			actual:   "cloud-hypervisor",
			expected: "",
			want:     false,
		},
		{
			name:     "empty actual",
			actual:   "",
			expected: "cloud-hypervisor",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := processNameMatches(tt.actual, tt.expected)
			if got != tt.want {
				t.Fatalf("processNameMatches(%q, %q)=%v, want %v", tt.actual, tt.expected, got, tt.want)
			}
		})
	}
}
