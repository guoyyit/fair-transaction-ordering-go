package fairorder

import (
	"bytes"
	"testing"
)

func TestCommitmentVerify(t *testing.T) {
	salt := []byte("s3cr3t-salt")
	payload := []byte(`{"to":"bob","amount":100}`)
	c := MakeCommitment(salt, payload)
	if len(c) != 32 {
		t.Fatalf("commitment len = %d, want 32", len(c))
	}
	if !VerifyCommitment(c, salt, payload) {
		t.Fatal("valid commitment failed verification")
	}
	if VerifyCommitment(c, []byte("wrong-salt"), payload) {
		t.Fatal("wrong salt accepted")
	}
	if VerifyCommitment(c, salt, []byte(`{"to":"eve","amount":100}`)) {
		t.Fatal("tampered payload accepted")
	}
	if VerifyCommitment([]byte("short"), salt, payload) {
		t.Fatal("short commitment accepted")
	}
}

func TestCommitmentBindingNoAmbiguity(t *testing.T) {
	// 长度前缀编码：不同的 salt/payload 切分不得产生相同承诺。
	a := MakeCommitment([]byte("ab"), []byte("c"))
	b := MakeCommitment([]byte("a"), []byte("bc"))
	if bytes.Equal(a, b) {
		t.Fatal("length-prefix encoding failed: ambiguous commitments equal")
	}
}

func TestOrderKeyDeterministic(t *testing.T) {
	p := []byte("tx-body")
	if !bytes.Equal(OrderKey(p), OrderKey(p)) {
		t.Fatal("order key not deterministic")
	}
	if bytes.Equal(OrderKey([]byte("a")), OrderKey([]byte("b"))) {
		t.Fatal("distinct payloads got same order key")
	}
}
