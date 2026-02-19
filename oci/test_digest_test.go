package oci

import "fmt"

func testHexDigest(n int) string {
	return fmt.Sprintf("%064x", n)
}
