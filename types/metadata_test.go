package types

import "testing"

func TestHypervisorProcessName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		meta         *VMMetadataFile
		configBinary string
		want         string
	}{
		{
			name:         "metadata field set",
			meta:         &VMMetadataFile{HypervisorBinary: "my-hypervisor"},
			configBinary: "cloud-hypervisor",
			want:         "my-hypervisor",
		},
		{
			name:         "metadata empty, config set",
			meta:         &VMMetadataFile{},
			configBinary: "custom-ch",
			want:         "custom-ch",
		},
		{
			name:         "both empty, returns default",
			meta:         &VMMetadataFile{},
			configBinary: "",
			want:         DefaultHypervisorProcess,
		},
		{
			name:         "config is absolute path, returns basename",
			meta:         &VMMetadataFile{},
			configBinary: "/usr/local/bin/cloud-hypervisor",
			want:         "cloud-hypervisor",
		},
		{
			name:         "config is relative path, returns basename",
			meta:         &VMMetadataFile{},
			configBinary: "./bin/my-ch",
			want:         "my-ch",
		},
		{
			name:         "config with whitespace, returns trimmed basename",
			meta:         &VMMetadataFile{},
			configBinary: "  /usr/bin/ch  ",
			want:         "ch",
		},
		{
			name:         "nil metadata, config set",
			meta:         nil,
			configBinary: "custom-ch",
			want:         "custom-ch",
		},
		{
			name:         "nil metadata, config is path",
			meta:         nil,
			configBinary: "/opt/bin/cloud-hypervisor",
			want:         "cloud-hypervisor",
		},
		{
			name:         "nil metadata, config empty",
			meta:         nil,
			configBinary: "",
			want:         DefaultHypervisorProcess,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.meta.HypervisorProcessName(tt.configBinary)
			if got != tt.want {
				t.Errorf("HypervisorProcessName(%q) = %q, want %q", tt.configBinary, got, tt.want)
			}
		})
	}
}

func TestVirtiofsdProcessName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		meta         *VMMetadataFile
		configBinary string
		want         string
	}{
		{
			name:         "metadata field set",
			meta:         &VMMetadataFile{VirtiofsdBinary: "my-virtiofsd"},
			configBinary: "virtiofsd",
			want:         "my-virtiofsd",
		},
		{
			name:         "metadata empty, config set",
			meta:         &VMMetadataFile{},
			configBinary: "custom-virtiofsd",
			want:         "custom-virtiofsd",
		},
		{
			name:         "both empty, returns default",
			meta:         &VMMetadataFile{},
			configBinary: "",
			want:         DefaultVirtiofsdProcess,
		},
		{
			name:         "config is absolute path, returns basename",
			meta:         &VMMetadataFile{},
			configBinary: "/usr/libexec/virtiofsd",
			want:         "virtiofsd",
		},
		{
			name:         "config with whitespace, returns trimmed basename",
			meta:         &VMMetadataFile{},
			configBinary: "  /usr/bin/virtiofsd-rs  ",
			want:         "virtiofsd-rs",
		},
		{
			name:         "nil metadata, config empty",
			meta:         nil,
			configBinary: "",
			want:         DefaultVirtiofsdProcess,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.meta.VirtiofsdProcessName(tt.configBinary)
			if got != tt.want {
				t.Errorf("VirtiofsdProcessName(%q) = %q, want %q", tt.configBinary, got, tt.want)
			}
		})
	}
}
