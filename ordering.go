package fairorder

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"time"
)

// 排序规则（公开、确定、可独立校验）：
//
//  1. 只有承诺与揭示内容一致、且身份为 (sender, nonce) 唯一的有效揭示参与排序；
//  2. 主排序键为 order_key = SHA256(payload)，按字节升序；
//  3. order_key 相同（即交易原文相同）时，以 commitment 哈希升序作确定性
//     平局裁决；仍相同时以 (sender, nonce) 升序兜底。
//
// 该规则完全不依赖提交/揭示的到达时间或调用顺序，因此任何参与方都无法通过
// 延迟或撤回揭示来改变自己的相对顺序；同一轮在任意并发到达顺序下结果唯一，
// 且任何持有 (payload, salt, commitment) 的人都可以离线复算整个顺序。

type revealEntry struct {
	rec    RevealRecord
	commit CommitRecord
}

func compareBytes(a, b []byte) int { return bytes.Compare(a, b) }

// buildOrder 依据全部有效揭示构建确定性最终顺序。
// 调用方保证 commits 与 reveals 属于同一轮，且每条 reveal 都有匹配的 commit。
func buildOrder(rc RoundConfig, commits []CommitRecord, reveals []RevealRecord, finalizedAt time.Time) *OrderResult {
	commitByKey := make(map[string]CommitRecord, len(commits))
	for _, c := range commits {
		commitByKey[commitKey(c.Sender, c.Nonce)] = c
	}

	entries := make([]revealEntry, 0, len(reveals))
	revealed := make(map[string]bool, len(reveals))
	for _, rv := range reveals {
		k := commitKey(rv.Sender, rv.Nonce)
		revealed[k] = true
		c, ok := commitByKey[k]
		if !ok {
			// 理论上不会发生：有效揭示必须先有承诺。防御性跳过。
			continue
		}
		entries = append(entries, revealEntry{rec: rv, commit: c})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if cmp := bytes.Compare(OrderKey(a.rec.Payload), OrderKey(b.rec.Payload)); cmp != 0 {
			return cmp < 0
		}
		if cmp := bytes.Compare(a.commit.Commitment, b.commit.Commitment); cmp != 0 {
			return cmp < 0
		}
		if a.rec.Sender != b.rec.Sender {
			return a.rec.Sender < b.rec.Sender
		}
		return a.rec.Nonce < b.rec.Nonce
	})

	txs := make([]OrderedTransaction, 0, len(entries))
	for _, e := range entries {
		txs = append(txs, OrderedTransaction{
			Sender:     e.rec.Sender,
			Nonce:      e.rec.Nonce,
			OrderKey:   OrderKey(e.rec.Payload),
			Commitment: e.commit.Commitment,
			Payload:    append([]byte(nil), e.rec.Payload...),
		})
	}

	excluded := make([]ExcludedCommit, 0)
	for _, c := range commits {
		if revealed[commitKey(c.Sender, c.Nonce)] {
			continue
		}
		excluded = append(excluded, ExcludedCommit{
			Sender: c.Sender,
			Nonce:  c.Nonce,
			Reason: ReasonUnrevealed,
		})
	}
	// 排除列表也按 (sender, nonce) 确定序，保证结果整体可复现。
	sort.Slice(excluded, func(i, j int) bool {
		if excluded[i].Sender != excluded[j].Sender {
			return excluded[i].Sender < excluded[j].Sender
		}
		return excluded[i].Nonce < excluded[j].Nonce
	})

	return &OrderResult{
		Round:        rc.ID,
		Transactions: txs,
		Excluded:     excluded,
		FinalizedAt:  finalizedAt,
	}
}

// VerifyOrderResult 独立校验一个已确认结果：
//
//  1. 每个条目的 OrderKey 必须等于 SHA256(payload)；
//  2. 条目必须严格按 (order_key, commitment, sender, nonce) 升序，
//     即与公开排序规则完全一致；
//  3. 条目身份 (sender, nonce) 不得重复。
//
// 承诺与盐值的对应关系可在揭示数据可用时用 VerifyCommitment 另行校验；
// 承诺本身在提交阶段已不可逆绑定，结果中一并公布以便审计。
func VerifyOrderResult(res *OrderResult) error {
	if res == nil {
		return errors.New("fairorder: nil result")
	}
	seen := make(map[string]bool, len(res.Transactions))
	for i, tx := range res.Transactions {
		if string(OrderKey(tx.Payload)) != string(tx.OrderKey) {
			return fmt.Errorf("fairorder: order key mismatch at index %d (%s/%d)", i, tx.Sender, tx.Nonce)
		}
		k := commitKey(tx.Sender, tx.Nonce)
		if seen[k] {
			return fmt.Errorf("fairorder: duplicate transaction in result at index %d", i)
		}
		seen[k] = true
		if i == 0 {
			continue
		}
		prev := res.Transactions[i-1]
		if c := bytes.Compare(prev.OrderKey, tx.OrderKey); c > 0 {
			return fmt.Errorf("fairorder: result not sorted by order key at index %d", i)
		} else if c == 0 {
			if c2 := bytes.Compare(prev.Commitment, tx.Commitment); c2 > 0 {
				return fmt.Errorf("fairorder: tie-break by commitment violated at index %d", i)
			} else if c2 == 0 {
				if prev.Sender > tx.Sender || (prev.Sender == tx.Sender && prev.Nonce > tx.Nonce) {
					return fmt.Errorf("fairorder: tie-break by identity violated at index %d", i)
				}
			}
		}
	}
	return nil
}
