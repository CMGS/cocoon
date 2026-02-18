package oci

import (
	"testing"
)

func TestParseCocoonfile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		wantFrom  string
		wantSteps int
		wantErr   bool
	}{
		{
			name: "basic Cocoonfile",
			input: `FROM ubuntu-jammy-cloudimg-amd64.img
RUN apt-get update && apt-get install -y nginx
COPY ./app /opt/app
LABEL version="1.0"
`,
			wantFrom:  "ubuntu-jammy-cloudimg-amd64.img",
			wantSteps: 2, // 1 RUN + 1 COPY
		},
		{
			name: "case insensitive directives",
			input: `from ubuntu.img
run echo hello
copy ./src /dst
label env=prod
`,
			wantFrom:  "ubuntu.img",
			wantSteps: 2,
		},
		{
			name: "comments and blank lines",
			input: `# This is a comment
FROM base.img

# Another comment
RUN echo hello
`,
			wantFrom:  "base.img",
			wantSteps: 1,
		},
		{
			name: "line continuation",
			input: `FROM base.img
RUN apt-get update && \
apt-get install -y nginx
`,
			wantFrom:  "base.img",
			wantSteps: 1,
		},
		{
			name: "registry-like FROM with dot in host",
			input: `FROM myregistry.io/ubuntu-vm:22.04
RUN echo done
`,
			wantFrom:  "myregistry.io/ubuntu-vm:22.04",
			wantSteps: 1,
		},
		{
			name: "URL FROM rejected",
			input: `FROM https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img
`,
			wantErr: true,
		},
		{
			name:    "missing FROM",
			input:   `RUN echo hello`,
			wantErr: true,
		},
		{
			name:    "empty FROM",
			input:   `FROM`,
			wantErr: true,
		},
		{
			name: "multiple FROM",
			input: `FROM base1.img
FROM base2.img
`,
			wantErr: true,
		},
		{
			name:    "unknown directive",
			input:   "FROM base.img\nENV FOO=bar\n",
			wantErr: true,
		},
		{
			name:    "COPY missing dest",
			input:   "FROM base.img\nCOPY ./src\n",
			wantErr: true,
		},
		{
			name:    "empty RUN",
			input:   "FROM base.img\nRUN\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cf, err := ParseCocoonfileFromString(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cf.From != tt.wantFrom {
				t.Errorf("From = %q, want %q", cf.From, tt.wantFrom)
			}
			if len(cf.Steps) != tt.wantSteps {
				t.Errorf("Steps = %d, want %d", len(cf.Steps), tt.wantSteps)
			}
		})
	}
}

func TestParseCocoonfileStepTypes(t *testing.T) {
	t.Parallel()

	input := `FROM base.img
RUN apt-get update
COPY ./app /opt/app
RUN systemctl enable nginx
COPY ./config.ini /etc/app/config.ini
`
	cf, err := ParseCocoonfileFromString(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cf.Steps) != 4 {
		t.Fatalf("Steps = %d, want 4", len(cf.Steps))
	}

	wantSteps := []Step{
		{Type: "run", Args: "apt-get update"},
		{Type: "copy", Args: "./app /opt/app"},
		{Type: "run", Args: "systemctl enable nginx"},
		{Type: "copy", Args: "./config.ini /etc/app/config.ini"},
	}

	for i, want := range wantSteps {
		got := cf.Steps[i]
		if got.Type != want.Type {
			t.Errorf("Step[%d].Type = %q, want %q", i, got.Type, want.Type)
		}
		if got.Args != want.Args {
			t.Errorf("Step[%d].Args = %q, want %q", i, got.Args, want.Args)
		}
	}
}

func TestParseCocoonfileLabels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantLabels map[string]string
		wantErr    bool
	}{
		{
			name: "simple label",
			input: `FROM base.img
LABEL version=1.0
`,
			wantLabels: map[string]string{"version": "1.0"},
		},
		{
			name: "quoted label",
			input: `FROM base.img
LABEL description="My cool VM image"
`,
			wantLabels: map[string]string{"description": "My cool VM image"},
		},
		{
			name: "multiple labels on one line",
			input: `FROM base.img
LABEL version=1.0 env=prod
`,
			wantLabels: map[string]string{"version": "1.0", "env": "prod"},
		},
		{
			name: "multiple LABEL directives",
			input: `FROM base.img
LABEL version=1.0
LABEL maintainer="john@example.com"
`,
			wantLabels: map[string]string{
				"version":    "1.0",
				"maintainer": "john@example.com",
			},
		},
		{
			name:    "invalid label format",
			input:   "FROM base.img\nLABEL noequals\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cf, err := ParseCocoonfileFromString(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for k, want := range tt.wantLabels {
				got, ok := cf.Labels[k]
				if !ok {
					t.Errorf("label %q not found", k)
					continue
				}
				if got != want {
					t.Errorf("label %q = %q, want %q", k, got, want)
				}
			}
			if len(cf.Labels) != len(tt.wantLabels) {
				t.Errorf("Labels count = %d, want %d", len(cf.Labels), len(tt.wantLabels))
			}
		})
	}
}

func TestParseCocoonfileLineContinuation(t *testing.T) {
	t.Parallel()

	input := `FROM base.img
RUN apt-get update && \
apt-get install -y \
nginx curl
`
	cf, err := ParseCocoonfileFromString(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cf.Steps) != 1 {
		t.Fatalf("Steps = %d, want 1", len(cf.Steps))
	}

	wantArgs := "apt-get update && apt-get install -y nginx curl"
	if cf.Steps[0].Args != wantArgs {
		t.Errorf("Step.Args = %q, want %q", cf.Steps[0].Args, wantArgs)
	}
}
