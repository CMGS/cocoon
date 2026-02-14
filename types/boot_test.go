package types

import "testing"

func TestParseBootStrategy_ValidValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want BootStrategy
	}{
		{in: "uefi_only", want: BootStrategyUEFIOnly},
		{in: "pvh_only", want: BootStrategyPVHOnly},
		{in: "  PVH_ONLY  ", want: BootStrategyPVHOnly},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseBootStrategy(tc.in)
			if err != nil {
				t.Fatalf("ParseBootStrategy(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseBootStrategy(%q)=%q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseBootStrategy_EmptyUsesDefault(t *testing.T) {
	t.Parallel()

	got, err := ParseBootStrategy("")
	if err != nil {
		t.Fatalf("ParseBootStrategy(empty) error: %v", err)
	}
	if got != DefaultBootStrategy {
		t.Fatalf("ParseBootStrategy(empty)=%q, want default %q", got, DefaultBootStrategy)
	}
}

func TestParseBootStrategy_Invalid(t *testing.T) {
	t.Parallel()

	if _, err := ParseBootStrategy("direct_kernel"); err == nil {
		t.Fatal("ParseBootStrategy(invalid) should return error")
	}
}
