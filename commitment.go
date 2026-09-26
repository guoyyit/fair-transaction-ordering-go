package fairorder

import (
	"crypto/sha256"
	"encoding/binary"
)

// commitDomain separates commitment hashes from all other hashes.
const commitDomain = "fairorder/commit/v1"

// ComputeCommitment derives the irreversible commitment for a payload.
//
// The commitment binds the round id, so a commitment made for one round
// can never be replayed into another round:
//
//	SHA-256("fairorder/commit/v1" || roundID(be64) || salt || payload)
//
// The salt must be a fresh random 32-byte value per transaction; without
// it the payload could be brute-forced from the commitment.
func ComputeCommitment(round RoundID, payload []byte, salt Salt) Commitment {
	h := sha256.New()
	h.Write([]byte(commitDomain))
	var rb [8]byte
	binary.BigEndian.PutUint64(rb[:], uint64(round))
	h.Write(rb[:])
	h.Write(salt[:])
	h.Write(payload)
	var c Commitment
	copy(c[:], h.Sum(nil))
	return c
}

// VerifyCommitment reports whether payload+salt open the commitment.
func VerifyCommitment(round RoundID, c Commitment, payload []byte, salt Salt) bool {
	return ComputeCommitment(round, payload, salt) == c
}
