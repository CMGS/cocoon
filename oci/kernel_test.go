package oci

import (
	"testing"
)

func TestDetectKernel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		bootFiles   []string
		wantVersion string
		wantKernel  string
		wantInitrd  string
		wantErr     bool
	}{
		{
			name: "Debian-style single kernel",
			bootFiles: []string{
				"/boot/vmlinuz-5.15.0-100-generic",
				"/boot/initrd.img-5.15.0-100-generic",
				"/boot/grub",
				"/boot/config-5.15.0-100-generic",
			},
			wantVersion: "5.15.0-100-generic",
			wantKernel:  "/boot/vmlinuz-5.15.0-100-generic",
			wantInitrd:  "/boot/initrd.img-5.15.0-100-generic",
		},
		{
			name: "Debian-style multiple kernels picks highest",
			bootFiles: []string{
				"/boot/vmlinuz-5.15.0-91-generic",
				"/boot/vmlinuz-5.15.0-100-generic",
				"/boot/vmlinuz-5.4.0-42-generic",
				"/boot/initrd.img-5.15.0-91-generic",
				"/boot/initrd.img-5.15.0-100-generic",
				"/boot/initrd.img-5.4.0-42-generic",
			},
			wantVersion: "5.15.0-100-generic",
			wantKernel:  "/boot/vmlinuz-5.15.0-100-generic",
			wantInitrd:  "/boot/initrd.img-5.15.0-100-generic",
		},
		{
			name: "RHEL-style initramfs",
			bootFiles: []string{
				"/boot/vmlinuz-5.14.0-362.el9.x86_64",
				"/boot/initramfs-5.14.0-362.el9.x86_64.img",
			},
			wantVersion: "5.14.0-362.el9.x86_64",
			wantKernel:  "/boot/vmlinuz-5.14.0-362.el9.x86_64",
			wantInitrd:  "/boot/initramfs-5.14.0-362.el9.x86_64.img",
		},
		{
			name: "no kernel found",
			bootFiles: []string{
				"/boot/grub",
				"/boot/config-5.15.0-100-generic",
			},
			wantErr: true,
		},
		{
			name: "kernel but no matching initrd",
			bootFiles: []string{
				"/boot/vmlinuz-5.15.0-100-generic",
			},
			wantErr: true,
		},
		{
			name:      "empty boot files",
			bootFiles: []string{},
			wantErr:   true,
		},
		{
			name: "vmlinuz without version suffix is ignored when versioned exists",
			bootFiles: []string{
				"/boot/vmlinuz",
				"/boot/vmlinuz-5.15.0-100-generic",
				"/boot/initrd.img-5.15.0-100-generic",
			},
			wantVersion: "5.15.0-100-generic",
			wantKernel:  "/boot/vmlinuz-5.15.0-100-generic",
			wantInitrd:  "/boot/initrd.img-5.15.0-100-generic",
		},
		{
			name: "bare vmlinuz fallback (Alpine-style)",
			bootFiles: []string{
				"/boot/vmlinuz",
				"/boot/initrd.img",
				"/boot/grub",
			},
			wantVersion: "unknown",
			wantKernel:  "/boot/vmlinuz",
			wantInitrd:  "/boot/initrd.img",
		},
		{
			name: "bare vmlinuz with initramfs.img",
			bootFiles: []string{
				"/boot/vmlinuz",
				"/boot/initramfs.img",
			},
			wantVersion: "unknown",
			wantKernel:  "/boot/vmlinuz",
			wantInitrd:  "/boot/initramfs.img",
		},
		{
			name: "ARM64 Image kernel fallback",
			bootFiles: []string{
				"/boot/Image",
				"/boot/initrd.img",
			},
			wantVersion: "unknown",
			wantKernel:  "/boot/Image",
			wantInitrd:  "/boot/initrd.img",
		},
		{
			name: "bare vmlinuz with plain initrd",
			bootFiles: []string{
				"/boot/vmlinuz",
				"/boot/initrd",
			},
			wantVersion: "unknown",
			wantKernel:  "/boot/vmlinuz",
			wantInitrd:  "/boot/initrd",
		},
		{
			name: "bare vmlinuz with plain initramfs",
			bootFiles: []string{
				"/boot/vmlinuz",
				"/boot/initramfs",
			},
			wantVersion: "unknown",
			wantKernel:  "/boot/vmlinuz",
			wantInitrd:  "/boot/initramfs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ki, err := DetectKernel(tt.bootFiles)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ki.Version != tt.wantVersion {
				t.Errorf("Version = %q, want %q", ki.Version, tt.wantVersion)
			}
			if ki.KernelPath != tt.wantKernel {
				t.Errorf("KernelPath = %q, want %q", ki.KernelPath, tt.wantKernel)
			}
			if ki.InitrdPath != tt.wantInitrd {
				t.Errorf("InitrdPath = %q, want %q", ki.InitrdPath, tt.wantInitrd)
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		a, b string
		want int // >0 means a > b, <0 means a < b, 0 means equal
	}{
		{"5.15.0-100", "5.15.0-91", 1},
		{"5.15.0-91", "5.15.0-100", -1},
		{"5.15.0", "5.15.0", 0},
		{"5.15.0", "5.4.0", 1},
		{"5.4.0", "5.15.0", -1},
		{"6.1.0", "5.15.0", 1},
		{"5.15.0-100-generic", "5.15.0-91-generic", 1},
		{"5.14.0-362.el9.x86_64", "5.14.0-100.el9.x86_64", 1},
	}

	for _, tt := range tests {
		t.Run(tt.a+"_vs_"+tt.b, func(t *testing.T) {
			t.Parallel()
			got := compareVersions(tt.a, tt.b)
			if (tt.want > 0 && got <= 0) || (tt.want < 0 && got >= 0) || (tt.want == 0 && got != 0) {
				t.Errorf("compareVersions(%q, %q) = %d, want sign %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
