package engine

import (
	"context"
	"os"
	"testing"
)

// Keep vm/engine tests hermetic: resolver registry probes should not depend on
// host skopeo availability or network.
func TestMain(m *testing.M) {
	runSkopeoInspectRaw = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`), nil
	}
	os.Exit(m.Run())
}
