package engine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// Keep vm/engine tests hermetic: resolver registry probes should not depend on
// host skopeo availability or network.
func TestMain(m *testing.M) {
	runSkopeoInspectRaw = func(_ context.Context, ref string, _ string) ([]byte, error) {
		switch {
		case strings.Contains(ref, "probe-error"):
			return nil, errors.New("network timeout")
		case strings.Contains(ref, "cocoon-vm"):
			return []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"application/vnd.cocoon.vm.image.v1","config":{"mediaType":"application/vnd.cocoon.vm.config.v1+json"},"layers":[{"mediaType":"application/vnd.cocoon.vm.kernel.v1.tar"},{"mediaType":"application/vnd.cocoon.vm.rootfs.v1.tar"}]}`), nil
		}
		return []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`), nil
	}
	os.Exit(m.Run())
}
