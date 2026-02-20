package utils

import "testing"

func TestParseSemVersion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want SemVersion
		ok   bool
	}{
		{
			name: "three parts",
			in:   "cloud-hypervisor 38.0.1",
			want: SemVersion{Major: 38, Minor: 0, Patch: 1},
			ok:   true,
		},
		{
			name: "two parts",
			in:   "virtiofsd 1.9",
			want: SemVersion{Major: 1, Minor: 9, Patch: 0},
			ok:   true,
		},
		{
			name: "invalid",
			in:   "unknown version",
			ok:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseSemVersion(tc.in)
			if tc.ok {
				if err != nil {
					t.Fatalf("ParseSemVersion(%q) error: %v", tc.in, err)
				}
				if got != tc.want {
					t.Fatalf("ParseSemVersion(%q) = %+v, want %+v", tc.in, got, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseSemVersion(%q) expected error, got nil", tc.in)
			}
		})
	}
}

func TestCompareSemVersion(t *testing.T) {
	t.Parallel()

	if got := CompareSemVersion(SemVersion{Major: 1, Minor: 2, Patch: 3}, SemVersion{Major: 1, Minor: 2, Patch: 3}); got != 0 {
		t.Fatalf("equal compare = %d, want 0", got)
	}
	if got := CompareSemVersion(SemVersion{Major: 1, Minor: 2, Patch: 4}, SemVersion{Major: 1, Minor: 2, Patch: 3}); got != 1 {
		t.Fatalf("greater compare = %d, want 1", got)
	}
	if got := CompareSemVersion(SemVersion{Major: 1, Minor: 1, Patch: 9}, SemVersion{Major: 1, Minor: 2, Patch: 0}); got != -1 {
		t.Fatalf("less compare = %d, want -1", got)
	}
}
