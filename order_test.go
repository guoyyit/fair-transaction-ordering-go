package fairorder

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math/rand"
	"sort"
	"testing"
)

func revealedSet(t *testing.T, round RoundID, n int) []RevealedTx {
	t.Helper()
	txs := make([]RevealedTx, n)
	for i := range txs {
		payload := []byte{byte(i), byte(i >> 8)}
		salt := makeSalt(t, byte(i+1))
		txs[i] = RevealedTx{
			Commitment: ComputeCommitment(round, payload, salt),
			Payload:    payload,
			Salt:       salt,
		}
	}
	return txs
}

func TestSortRevealedDeterministicUnderShuffle(t *testing.T) {
	txs := revealedSet(t, 7, 64)
	want := SortRevealed(7, txs)

	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 20; trial++ {
		shuffled := make([]RevealedTx, len(txs))
		copy(shuffled, txs)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got := SortRevealed(7, shuffled)
		for i := range want {
			if got[i].Commitment != want[i].Commitment {
				t.Fatalf("trial %d: position %d differs", trial, i)
			}
		}
	}
}

// TestOrderKeyIndependentVerification recomputes the ordering with nothing
// but the public rule (domain string, round id, commitment) and SHA-256.
func TestOrderKeyIndependentVerification(t *testing.T) {
	const round = 3
	txs := revealedSet(t, round, 32)
	got := SortRevealed(round, txs)

	type keyed struct {
		key [32]byte
		c   Commitment
	}
	ks := make([]keyed, len(txs))
	for i, tx := range txs {
		h := sha256.New()
		h.Write([]byte("fairorder/order/v1"))
		var rb [8]byte
		binary.BigEndian.PutUint64(rb[:], round)
		h.Write(rb[:])
		h.Write(tx.Commitment[:])
		copy(ks[i].key[:], h.Sum(nil))
		ks[i].c = tx.Commitment
	}
	sort.Slice(ks, func(i, j int) bool {
		if c := bytes.Compare(ks[i].key[:], ks[j].key[:]); c != 0 {
			return c < 0
		}
		return bytes.Compare(ks[i].c[:], ks[j].c[:]) < 0
	})
	for i := range got {
		if got[i].Commitment != ks[i].c {
			t.Fatalf("position %d: ordering not independently reproducible", i)
		}
	}
}

// TestWithholdingCannotChangeRelativeOrder: removing any subset of reveals
// leaves the relative order of the remainder untouched.
func TestWithholdingCannotChangeRelativeOrder(t *testing.T) {
	const round = 5
	txs := revealedSet(t, round, 32)
	full := SortRevealed(round, txs)

	// Withhold every other transaction.
	var subset []RevealedTx
	for i, tx := range txs {
		if i%2 == 0 {
			subset = append(subset, tx)
		}
	}
	partial := SortRevealed(round, subset)

	inSubset := make(map[Commitment]bool)
	for _, tx := range subset {
		inSubset[tx.Commitment] = true
	}
	var filtered []Commitment
	for _, tx := range full {
		if inSubset[tx.Commitment] {
			filtered = append(filtered, tx.Commitment)
		}
	}
	if len(filtered) != len(partial) {
		t.Fatalf("length mismatch: %d vs %d", len(filtered), len(partial))
	}
	for i := range partial {
		if partial[i].Commitment != filtered[i] {
			t.Fatalf("position %d: withholding changed relative order", i)
		}
	}
}
