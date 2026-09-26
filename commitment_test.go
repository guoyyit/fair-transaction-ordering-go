package fairorder

import (
	"crypto/rand"
	"testing"
)

func makeSalt(t *testing.T, seed byte) Salt {
	t.Helper()
	var s Salt
	for i := range s {
		s[i] = seed
	}
	return s
}

func randomSalt(t *testing.T) Salt {
	t.Helper()
	var s Salt
	if _, err := rand.Read(s[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return s
}

func TestCommitmentVerify(t *testing.T) {
	payload := []byte("transfer 1 ETH")
	salt := randomSalt(t)
	c := ComputeCommitment(1, payload, salt)

	if !VerifyCommitment(1, c, payload, salt) {
		t.Fatal("valid commitment rejected")
	}
	if VerifyCommitment(1, c, []byte("transfer 2 ETH"), salt) {
		t.Fatal("wrong payload accepted")
	}
	if VerifyCommitment(1, c, payload, makeSalt(t, 9)) {
		t.Fatal("wrong salt accepted")
	}
}

func TestCommitmentBindsRound(t *testing.T) {
	payload := []byte("tx")
	salt := randomSalt(t)
	c1 := ComputeCommitment(1, payload, salt)
	c2 := ComputeCommitment(2, payload, salt)
	if c1 == c2 {
		t.Fatal("commitments for different rounds must differ")
	}
	if VerifyCommitment(2, c1, payload, salt) {
		t.Fatal("commitment replayed across rounds")
	}
}
