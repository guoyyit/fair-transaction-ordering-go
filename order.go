package fairorder

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"sort"
)

// orderDomain separates ordering keys from all other hashes.
const orderDomain = "fairorder/order/v1"

// OrderKey is the public, reproducible sort key for a commitment in a round.
//
//	SHA-256("fairorder/order/v1" || roundID(be64) || commitment)
//
// Anyone can recompute the key and re-sort the revealed set to verify the
// final ordering independently. The key depends only on the round and the
// commitment, never on arrival time, so delaying or withholding a reveal
// cannot change the relative order of the remaining transactions.
func OrderKey(round RoundID, c Commitment) [32]byte {
	h := sha256.New()
	h.Write([]byte(orderDomain))
	var rb [8]byte
	binary.BigEndian.PutUint64(rb[:], uint64(round))
	h.Write(rb[:])
	h.Write(c[:])
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

// SortRevealed orders revealed transactions deterministically by ascending
// OrderKey. The input slice is not mutated; a new slice is returned.
func SortRevealed(round RoundID, txs []RevealedTx) []RevealedTx {
	out := make([]RevealedTx, len(txs))
	copy(out, txs)
	sort.SliceStable(out, func(i, j int) bool {
		ki, kj := OrderKey(round, out[i].Commitment), OrderKey(round, out[j].Commitment)
		if c := bytes.Compare(ki[:], kj[:]); c != 0 {
			return c < 0
		}
		// Defensive tie-break; identical keys imply identical commitments.
		return bytes.Compare(out[i].Commitment[:], out[j].Commitment[:]) < 0
	})
	return out
}

// VerifyResult independently checks that res is a correct deterministic
// outcome for the given round. Anyone holding the revealed transactions
// can run this without access to the service:
//
//   - every reveal must open its commitment in this round;
//   - no commitment may appear twice;
//   - the ordering must equal a fresh sort by the public OrderKey;
//   - timed-out commitments must not overlap the ordered set.
func VerifyResult(round RoundID, res RoundResult) error {
	if res.RoundID != round {
		return ErrResultRoundMismatch
	}
	seen := make(map[Commitment]struct{}, len(res.Ordered))
	for _, tx := range res.Ordered {
		if _, dup := seen[tx.Commitment]; dup {
			return ErrResultDuplicate
		}
		seen[tx.Commitment] = struct{}{}
		if !VerifyCommitment(round, tx.Commitment, tx.Payload, tx.Salt) {
			return ErrResultBadReveal
		}
	}
	sorted := SortRevealed(round, res.Ordered)
	for i := range sorted {
		if sorted[i].Commitment != res.Ordered[i].Commitment {
			return ErrResultBadOrder
		}
	}
	timedOut := make(map[Commitment]struct{}, len(res.TimedOut))
	for _, c := range res.TimedOut {
		if _, ok := seen[c]; ok {
			return ErrResultOverlap
		}
		if _, dup := timedOut[c]; dup {
			return ErrResultDuplicate
		}
		timedOut[c] = struct{}{}
	}
	return nil
}
