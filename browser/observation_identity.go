package browser

import (
	"crypto/rand"
	"encoding/hex"
)

// NewObservationID assigns a globally unique capture identity. It is generated
// once at explicit runtime capture, then shared by refs, artifacts and adapters.
func NewObservationID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(bytes[:])
}
