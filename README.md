# fair-transaction-ordering-go

本地交易排序服务：在区块打包前以 **承诺–揭示（commit–reveal）** 协议确定交易顺序，抑制抢跑（front-running）。纯 Go 标准库实现，无外部服务依赖。

## 工作流程

每一轮（round）分为三个阶段，时间窗口由 `Config` 配置：

```
|------ CommitWindow ------|------ RevealWindow ------|
   提交承诺（不可见原文）        揭示原文并校验            Finalize 产出最终顺序
```

1. **提交（Commit）**：客户端本地计算承诺并提交
   `Commitment = SHA-256("fairorder/commit/v1" || roundID(be64) || salt || payload)`。
   承诺绑定轮次，跨轮重放无效；`salt` 必须为每笔交易新生成的 32 字节随机数。
2. **揭示（Reveal）**：客户端提交 `(commitment, payload, salt)`，服务重新计算承诺并校验对应关系，不匹配即拒绝（`ErrCommitmentMismatch`）。
3. **定序（Finalize）**：揭示截止后，全部有效揭示按公开规则排序产出 `RoundResult`。

## 公平性语义

- **排序规则（公开、可复现）**：每笔交易的关键字为
  `OrderKey = SHA-256("fairorder/order/v1" || roundID(be64) || commitment)`，
  最终顺序为 OrderKey 升序（并列时按 commitment 字节升序，实际不会发生）。
- **到达顺序无关**：排序只取决于"截止前有效揭示的集合"，与提交/揭示的到达时间、并发交错完全无关。并发测试在 `-race` 下验证多次运行结果一致。
- **延迟/撤回无法改变相对顺序**：关键字只含轮次与承诺。延迟揭示（在窗口内）不影响结果；撤回揭示只会移除自己，其余交易的相对顺序保持不变（`TestWithholdingCannotChangeRelativeOrder`）。
- **抢跑抑制**：承诺在提交阶段不暴露原文；揭示阶段开始后不再接受新承诺（`ErrCommitPhaseOver`），攻击者看到受害者揭示后无法在同一轮插入跟风交易，也无法伪造他人承诺的揭示。
- **一次性**：重复提交（`ErrDuplicateCommit`）与重复揭示（`ErrDuplicateReveal`）被识别并拒绝；超时未揭示的承诺被排除在最终顺序之外，按 `UnrevealedPolicy` 处理：
  - `UnrevealedExclude`：静默丢弃；
  - `UnrevealedFlag`：丢弃但在 `RoundResult.TimedOut` 中列出（按 OrderKey 排序，便于审计）。

## 独立校验

任何持有揭示集合的一方无需访问服务即可验证结果：

```go
if err := fairorder.VerifyResult(roundID, result); err != nil { /* 结果被篡改 */ }
```

`VerifyResult` 检查：每个揭示都能打开其承诺、无重复承诺、顺序等于按公开规则重新排序的结果、超时集与有序集无交集。`OrderKey`/`SortRevealed`/`ComputeCommitment` 均为导出函数，可用任意语言独立复算（测试中以原始 `crypto/sha256` 独立重算验证）。

## 持久化与崩溃恢复

- 每个改变状态的操作（开轮、提交、揭示、定案、废弃）在返回成功**之前**追加一条 WAL 事件；`FileStore` 每次追加后 `fsync`。已确认的结果不会因崩溃丢失。
- WAL 为 JSON-lines 文件（承诺与盐以 hex 编码），可直接查看、独立审计。
- 重启时重放 WAL 重建状态：重复提交仍被拒绝、已揭示交易不丢失、已定案结果原样保留、不会重复纳入交易。
- 崩溃时未完成轮次按 `CrashPolicy` 处理：
  - `CrashResume`（默认）：以持久化的绝对截止时间继续；若停机期间已过揭示截止，可立即 `Finalize`（只纳入截止前已确认揭示）。
  - `CrashFinalize`：恢复时立即以当前有效揭示定案并持久化。
  - `CrashDiscard`：恢复时废弃该轮（`ErrRoundDiscarded`），不纳入任何交易。
- 崩溃导致的半行写入（torn tail）在加载时被忽略——按"fsync 后才确认"的规则，该事件从未被确认。

## 配置

```go
cfg := fairorder.Config{
    CommitWindow:     10 * time.Second, // 提交窗口（>0）
    RevealWindow:     10 * time.Second, // 揭示窗口（>0）
    UnrevealedPolicy: fairorder.UnrevealedFlag,
    CrashPolicy:      fairorder.CrashResume,
    MaxCommits:       0,                // 单轮承诺上限，0 为不限
}
svc, err := fairorder.NewService(cfg, store, clock) // store: FileStore/MemStore
```

注意：用同一 WAL 重启时 `Config` 的窗口参数必须一致（截止时间由持久化的开轮时间加窗口推导）。

## API 摘要

| 方法 | 说明 |
|---|---|
| `OpenRound() (RoundID, error)` | 开启新一轮（当前轮仍活跃则 `ErrRoundActive`） |
| `Submit(Commitment) error` | 提交窗口内提交承诺 |
| `Reveal(Commitment, payload, salt) error` | 揭示窗口内揭示并校验 |
| `Finalize() (RoundResult, error)` | 揭示截止后产出确定顺序 |
| `CurrentRound() (RoundInfo, error)` | 查询当前轮状态 |
| `Result(RoundID) (RoundResult, error)` | 查询已定案轮次结果 |

## 运行测试

```sh
go test ./...        # 全部测试
go test -race ./...  # 含竞态检测（并发排序一致性）
```

测试覆盖：抢跑尝试、重复提交/揭示、超时未揭示、并发排序一致性、跨轮隔离、崩溃恢复（三种策略）、torn tail 容错、配置与相位边界、结果独立校验与篡改检测。
