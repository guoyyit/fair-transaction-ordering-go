// Package fairorder 实现一个本地的提交-揭示（commit-reveal）式公平交易排序服务。
//
// 服务在区块打包前运行：提交阶段参与者只能公布不可逆的哈希承诺，揭示阶段
// 才公开交易原文与盐值；最终顺序由全部有效揭示按公开的确定性规则产生，
// 与提交/揭示的到达顺序、网络延迟无关，从而抑制抢跑（front-running）。
package fairorder

import (
	"errors"
	"time"
)

// RoundID 为一轮排序的唯一标识。
type RoundID string

// TimeoutPolicy 决定揭示截止时间到达后，对未揭示承诺的处理策略。
type TimeoutPolicy int

const (
	// PolicyExcludeUnrevealed：未揭示的承诺被剔除，轮次仅依据已验证有效的
	// 揭示正常完成并产出最终顺序。
	PolicyExcludeUnrevealed TimeoutPolicy = iota
	// PolicyAbort：存在未揭示承诺时，轮次按既定策略终止，不产出最终顺序。
	PolicyAbort
)

// RoundStatus 为轮次状态机状态。
type RoundStatus int

const (
	// StatusCommitting：提交阶段，只接受承诺。
	StatusCommitting RoundStatus = iota
	// StatusRevealing：揭示阶段，只接受与已存承诺匹配的揭示。
	StatusRevealing
	// StatusFinalized：轮次已完成，最终顺序已确定并持久化。
	StatusFinalized
	// StatusAborted：轮次按超时策略终止，未产出顺序。
	StatusAborted
)

func (s RoundStatus) String() string {
	switch s {
	case StatusCommitting:
		return "committing"
	case StatusRevealing:
		return "revealing"
	case StatusFinalized:
		return "finalized"
	case StatusAborted:
		return "aborted"
	default:
		return "unknown"
	}
}

// RoundConfig 描述一轮排序的起止时间与超时策略。
//
// 提交阶段为 [Now, CommitDeadline)，揭示阶段为
// [CommitDeadline, RevealDeadline)。到达 RevealDeadline 后，未揭示的提交
// 按 TimeoutPolicy 处理。
type RoundConfig struct {
	ID             RoundID
	CommitDeadline time.Time
	RevealDeadline time.Time
	TimeoutPolicy  TimeoutPolicy
}

// CommitRecord 是一笔提交（承诺）的持久化记录。
// 提交身份由 (Round, Sender, Nonce) 唯一确定，用于识别重复提交。
type CommitRecord struct {
	Round      RoundID   `json:"round"`
	Sender     string    `json:"sender"`
	Nonce      uint64    `json:"nonce"`
	Commitment []byte    `json:"commitment"`
	At         time.Time `json:"at"`
}

// RevealRecord 是一笔有效揭示的持久化记录。只有通过承诺校验的揭示才会落盘。
type RevealRecord struct {
	Round   RoundID   `json:"round"`
	Sender  string    `json:"sender"`
	Nonce   uint64    `json:"nonce"`
	Salt    []byte    `json:"salt"`
	Payload []byte    `json:"payload"`
	At      time.Time `json:"at"`
}

// ExcludeReason 标识一笔提交被排除出最终顺序的原因。
type ExcludeReason string

const (
	ReasonUnrevealed ExcludeReason = "unrevealed"
)

// ExcludedCommit 记录一笔未被纳入最终顺序的提交及其原因。
type ExcludedCommit struct {
	Sender string        `json:"sender"`
	Nonce  uint64        `json:"nonce"`
	Reason ExcludeReason `json:"reason"`
}

// OrderedTransaction 是最终顺序中的一个条目，可独立复算校验。
type OrderedTransaction struct {
	Sender string `json:"sender"`
	Nonce  uint64 `json:"nonce"`
	// OrderKey 为公开排序键：SHA256(规范化交易原文)，最终顺序按键升序。
	OrderKey []byte `json:"order_key"`
	// Commitment 为提交阶段公布的承诺，作为排序键相同时的确定性裁决依据。
	Commitment []byte `json:"commitment"`
	Payload    []byte `json:"payload"`
}

// OrderResult 为一轮排序的最终结果，落盘后即为已确认结果。
type OrderResult struct {
	Round        RoundID              `json:"round"`
	Transactions []OrderedTransaction `json:"transactions"`
	Excluded     []ExcludedCommit     `json:"excluded"`
	FinalizedAt  time.Time            `json:"finalized_at"`
}

// RoundInfo 为轮次的只读快照，供外部观察状态。
type RoundInfo struct {
	Config        RoundConfig
	Status        RoundStatus
	CommitCount   int
	RevealedCount int
}

// ServiceConfig 为排序服务的配置。
type ServiceConfig struct {
	// DataDir 为持久化目录；为空表示纯内存模式（不提供崩溃恢复）。
	DataDir string
	// Now 可注入时钟，默认使用 time.Now。
	Now func() time.Time
	// TickInterval 为内部调度检查间隔，默认 10ms。
	TickInterval time.Duration
}

// 服务与轮次操作可能返回的错误。
var (
	ErrRoundNotFound     = errors.New("fairorder: round not found")
	ErrRoundExists       = errors.New("fairorder: round already exists")
	ErrRoundClosed       = errors.New("fairorder: round is no longer accepting submissions")
	ErrCommitPhaseClosed = errors.New("fairorder: commit phase is closed")
	ErrRevealNotOpen     = errors.New("fairorder: reveal phase is not open")
	ErrDuplicateCommit   = errors.New("fairorder: duplicate commitment for (sender, nonce)")
	ErrCommitNotFound    = errors.New("fairorder: no matching commitment found for reveal")
	ErrDuplicateReveal   = errors.New("fairorder: valid reveal already submitted for (sender, nonce)")
	ErrInvalidCommitment = errors.New("fairorder: reveal does not match commitment")
	ErrRoundNotFinalized = errors.New("fairorder: round has not finalized yet")
	ErrRoundAborted      = errors.New("fairorder: round aborted by timeout policy")
	ErrInvalidConfig     = errors.New("fairorder: invalid round configuration")
)
