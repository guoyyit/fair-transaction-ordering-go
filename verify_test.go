package fairorder

import (
	"testing"
	"time"
)

func sampleResult(t *testing.T) *OrderResult {
	t.Helper()
	clock := &fakeClock{t: time.Unix(12000, 0)}
	svc := newTestService(t, clock)
	r, _ := svc.CreateRound(testConfig("v", clock.t, PolicyExcludeUnrevealed))
	pays := [][]byte{[]byte("aaa"), []byte("bbb"), []byte("ccc")}
	for i, p := range pays {
		s := []byte{byte(i + 1)}
		mustCommit(t, r, string(rune('a'+i)), 1, s, p)
	}
	clock.advance(100 * time.Millisecond)
	for i, p := range pays {
		s := []byte{byte(i + 1)}
		if err := r.SubmitReveal(RevealRecord{Sender: string(rune('a' + i)), Nonce: 1, Salt: s, Payload: p}); err != nil {
			t.Fatal(err)
		}
	}
	clock.advance(100 * time.Millisecond)
	res, err := r.Result()
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestVerifyOrderResultOK(t *testing.T) {
	if err := VerifyOrderResult(sampleResult(t)); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}
}

func TestVerifyOrderResultTampered(t *testing.T) {
	res := sampleResult(t)
	// 篡改某笔原文却不更新排序键：必须被发现。
	res.Transactions[0].Payload = []byte("tampered")
	if err := VerifyOrderResult(res); err == nil {
		t.Fatal("tampered payload not detected")
	}

	// 交换两个条目破坏顺序：必须被发现。
	res2 := sampleResult(t)
	res2.Transactions[0], res2.Transactions[len(res2.Transactions)-1] =
		res2.Transactions[len(res2.Transactions)-1], res2.Transactions[0]
	if err := VerifyOrderResult(res2); err == nil {
		t.Fatal("unsorted result not detected")
	}

	// 重复身份：必须被发现。
	res3 := sampleResult(t)
	res3.Transactions[1] = res3.Transactions[0]
	if err := VerifyOrderResult(res3); err == nil {
		t.Fatal("duplicate identity not detected")
	}
	if err := VerifyOrderResult(nil); err == nil {
		t.Fatal("nil result accepted")
	}
}
