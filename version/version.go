package version

import "fmt"

// These variables are set at build time via ldflags.
//
//	go build -ldflags "-X github.com/CMGS/cocoon/version.REVISION=$(git rev-parse HEAD) \
//	  -X github.com/CMGS/cocoon/version.VERSION=$(git describe --tags) \
//	  -X github.com/CMGS/cocoon/version.BUILTAT=$(date +%Y-%m-%dT%H:%M:%S)"
var (
	NAME     = "Cocoon"
	VERSION  = "unknown"
	REVISION = "HEAD"
	BUILTAT  = "now"
)

// String returns a human-readable version string.
func String() string {
	return fmt.Sprintf(
		"Version:    %s\nRevision:   %s\nBuilt at:   %s\n",
		VERSION, REVISION, BUILTAT,
	)
}
