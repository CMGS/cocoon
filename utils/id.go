package utils

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"time"

	"github.com/oklog/ulid/v2"
)

// NewVMID generates a new VM identifier using ULID.
// Format: vm-{ulid} (e.g., vm-01HXYZ5A3B7C8D9E0F1G2H3J4K).
// ULIDs are time-sortable and globally unique.
func NewVMID() string {
	id := ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader)
	return "vm-" + id.String()
}

// NewAutoName generates an auto-assigned VM name.
// Format: cocoon-{random-8-hex} (e.g., cocoon-a3f7b2c1).
func NewAutoName() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random name: %w", err)
	}
	return fmt.Sprintf("cocoon-%x", b), nil
}

// RandomHex returns n random hex characters.
func RandomHex(n int) (string, error) {
	const hexChars = "0123456789abcdef"
	result := make([]byte, n)
	for i := range result {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(hexChars))))
		if err != nil {
			return "", err
		}
		result[i] = hexChars[num.Int64()]
	}
	return string(result), nil
}
