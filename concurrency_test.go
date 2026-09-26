package fairorder

import (
	"bytes"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// TestConcurrentOrderingDeterministic：不同的随机调度（并发到达顺序不同）
// 必须产生字节完全一致的最终顺序。
func TestConcurrentOrderingDeterministic(t *testing.T) {
	const n = 40
	payloads := make([][]byte, n)
	salts := make([][]byte, n)
	for i := 0; i < n; i++ {
		payloads[i] = []byte(fmt.Sprintf("tx-body-%02d", i))
		salts[i] = []byte(fmt.Sprintf("salt-%02d", i))
	}
	// 预先算出期望顺序：公开规则下，直接按 SHA256(payload) 升序。
	want := make([]byte, 0)
	idxByKey := map[string]int{}
	keys := make([][]byte, n)
	for i := range payloads {
		keys[i] = OrderKey(payloads[i])
		idxByKey[string(keys[i])] = i
	}
	sortedKeys := append([][]byte(nil), keys...)
	sortBytes(sortedKeys)
	for _, k := range sortedKeys {
		want = append(want, k...)
	}

	runOnce := func(seed int64) []byte {
		clock := &fakeClock{t: time.Unix(2000, 0)}
		svc, err := NewService(ServiceConfig{Now: clock.now, TickInterval: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		defer svc.Close()
		r, err := svc.CreateRound(testConfig("c", clock.t, PolicyExcludeUnrevealed))
		if err != nil {
			t.Fatal(err)
		}
		rng := rand.New(rand.NewSource(seed))

		commitOrder := rng.Perm(n)
		commitJitter := make([]time.Duration, n)
		for i := range commitJitter {
			commitJitter[i] = time.Duration(rng.Intn(2)) * time.Microsecond
		}
		var wg sync.WaitGroup
		for pos, i := range commitOrder {
			wg.Add(1)
			go func(i int, d time.Duration) {
				defer wg.Done()
				time.Sleep(d)
				_ = r.SubmitCommit(CommitRecord{
					Sender:     fmt.Sprintf("p%02d", i),
					Nonce:      1,
					Commitment: MakeCommitment(salts[i], payloads[i]),
				})
			}(i, commitJitter[pos])
		}
		wg.Wait()

		clock.advance(100 * time.Millisecond)
		revealOrder := rng.Perm(n)
		revealJitter := make([]time.Duration, n)
		for i := range revealJitter {
			revealJitter[i] = time.Duration(rng.Intn(2)) * time.Microsecond
		}
		for pos, i := range revealOrder {
			wg.Add(1)
			go func(i int, d time.Duration) {
				defer wg.Done()
				time.Sleep(d)
				_ = r.SubmitReveal(RevealRecord{
					Sender:  fmt.Sprintf("p%02d", i),
					Nonce:   1,
					Salt:    salts[i],
					Payload: payloads[i],
				})
			}(i, revealJitter[pos])
		}
		wg.Wait()

		clock.advance(100 * time.Millisecond)
		res, err := r.Result()
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if len(res.Transactions) != n {
			t.Fatalf("seed %d: tx count = %d, want %d", seed, len(res.Transactions), n)
		}
		var got []byte
		for _, tx := range res.Transactions {
			got = append(got, tx.OrderKey...)
		}
		return got
	}

	first := runOnce(1)
	if !bytes.Equal(first, want) {
		t.Fatal("order does not match independently computed expected order")
	}
	for seed := int64(2); seed <= 8; seed++ {
		got := runOnce(seed)
		if !bytes.Equal(got, first) {
			t.Fatalf("order differs across concurrent schedules (seed %d)", seed)
		}
	}
}

// TestConcurrentDuplicateCommitOnce：同一身份并发重复提交，恰好一次成功。
func TestConcurrentDuplicateCommitOnce(t *testing.T) {
	clock := &fakeClock{t: time.Unix(3000, 0)}
	svc, err := NewService(ServiceConfig{Now: clock.now, TickInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	r, _ := svc.CreateRound(testConfig("dup", clock.t, PolicyExcludeUnrevealed))
	c := MakeCommitment([]byte("s"), []byte("p"))

	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := r.SubmitCommit(CommitRecord{Sender: "same", Nonce: 7, Commitment: c})
			mu.Lock()
			if err == nil {
				success++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("successful commits = %d, want exactly 1", success)
	}
	if r.Info().CommitCount != 1 {
		t.Fatalf("commit count = %d, want 1", r.Info().CommitCount)
	}
}

// TestFrontRunningResistance：抢跑者看到他人公开的承诺后，无法通过：
//   - 抢先提交相同承诺（身份不同，且不知道盐值无法揭示，最终被排除）；
//   - 篡改揭示内容（承诺校验失败被拒）；
//   - 延迟/撤回揭示改变相对顺序（顺序只由原文哈希决定）。
func TestFrontRunningResistance(t *testing.T) {
	clock := &fakeClock{t: time.Unix(4000, 0)}
	svc, err := NewService(ServiceConfig{Now: clock.now, TickInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	r, _ := svc.CreateRound(testConfig("fr", clock.t, PolicyExcludeUnrevealed))

	victimSalt := []byte("victim-secret-salt")
	victimPayload := []byte("BUY tokenX amount=50")
	victimCommit := MakeCommitment(victimSalt, victimPayload)

	// 受害者提交。
	mustCommit(t, r, "victim", 1, victimSalt, victimPayload)
	// 抢跑者复制受害者的承诺（哈希公开可见），试图抢先占位。
	if err := r.SubmitCommit(CommitRecord{Sender: "attacker", Nonce: 1, Commitment: victimCommit}); err != nil {
		t.Fatalf("attacker copy commit: %v", err)
	}
	// 抢跑者尝试用自己的原文揭示（盐值未知，必然不匹配）。
	clock.advance(100 * time.Millisecond)
	err = r.SubmitReveal(RevealRecord{Sender: "attacker", Nonce: 1, Salt: []byte("guessed"), Payload: []byte("BUY tokenX amount=50")})
	if err != ErrInvalidCommitment {
		t.Fatalf("attacker reveal should fail: got %v", err)
	}
	// 受害者正常揭示。
	if err := r.SubmitReveal(RevealRecord{Sender: "victim", Nonce: 1, Salt: victimSalt, Payload: victimPayload}); err != nil {
		t.Fatal(err)
	}
	clock.advance(100 * time.Millisecond)
	res, err := r.Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Transactions) != 1 || res.Transactions[0].Sender != "victim" {
		t.Fatalf("attacker affected ordering: %+v", res.Transactions)
	}
	if len(res.Excluded) != 1 || res.Excluded[0].Sender != "attacker" {
		t.Fatalf("attacker not marked excluded: %+v", res.Excluded)
	}
}

// sortBytes 按字典序排序字节切片切片。
func sortBytes(a [][]byte) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && bytes.Compare(a[j-1], a[j]) > 0; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}
