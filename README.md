# fair-transaction-ordering-go

一个**本地**运行的提交-揭示（commit-reveal）式公平交易排序服务，用于在区块打包前
确定交易顺序、抑制抢跑（front-running / MEV）。不依赖任何外部服务、数据库或网络
协调；仅使用 Go 标准库，持久化基于本地只追加日志。

## 1. 要解决的问题

若交易在提交时即公开原文，最先看到交易的一方（或排序者）可以插入自己的交易抢先
成交。本服务把每轮拆成两个阶段：

1. **提交阶段（commit）**：参与方只公布一个不可逆的哈希承诺
   `commitment = SHA256( len(salt) || salt || len(payload) || payload )`，
   其中 `payload` 是交易原文、`salt` 是当次随机盐。承诺公开后不可篡改、无法从承诺
   反推出原文，因此任何人在提交阶段都不知道他人交易内容，无法针对性抢跑。
2. **揭示阶段（reveal）**：参与方公开 `(salt, payload)`。服务用提交阶段的承诺校验
   `SHA256(len(salt)||salt||len(payload)||payload)` 是否一致，**不匹配的揭示一律
   拒绝**，不进入最终顺序。

到达揭示截止时间后，服务按公开的确定性规则对全部**有效揭示**排序并产出最终结果。

## 2. 公平性语义

- **承诺绑定**：承诺使用带 4 字节长度前缀的 SHA-256，既不可逆（抗原像），也避免
  `salt||payload` 拼接的边界歧义。提交后无法再修改原文；揭示与承诺不符即拒绝。
- **顺序与到达顺序无关**：最终顺序只取决于交易原文（及其提交承诺、身份），
  与提交/揭示的到达时刻、网络延迟、goroutine 调度无关。
- **延迟或撤回不能改变相对顺序**：
  - 排序键在提交内容确定时即已固定，揭示只是公开内容，早揭示晚揭示结果相同；
  - 撤回（超时不揭示）只会让该笔交易被**排除**或令整轮按策略**终止**，
    不会改变其余有效交易之间的相对顺序。
- **重复只计一次**：提交身份为 `(round, sender, nonce)`。重复提交、重复揭示都被
  识别并拒绝；最终顺序中每个身份至多出现一次。
- **公开、可复现、可独立校验**：排序规则完全公开，任何持有结果的人都能用
  `VerifyOrderResult` 或命令行工具 `cmd/orderverify` 离线复算，不需要运行服务。

### 排序规则（确定性）

1. 仅纳入：处于揭示阶段、且 `(salt, payload)` 通过承诺校验的唯一有效揭示；
2. 主键 `order_key = SHA256(payload)`，按字节**升序**；
3. `order_key` 相同（原文相同）时，以提交阶段的 `commitment` 字节升序裁决；
4. 仍相同时，以 `(sender, nonce)` 字典序/数值序兜底。

注意是按 **SHA256 摘要的字节序**而非原文字典序，因此结果不能通过对原文做
"恰好小一点"的微调来稳定抢到首位。

## 3. 轮次生命周期与超时策略

时间区间均为**左闭右开**：

| 阶段 | 时间区间 | 接受的操作 |
| --- | --- | --- |
| 提交 `committing` | `[开始, CommitDeadline)` | 仅提交承诺 |
| 揭示 `revealing` | `[CommitDeadline, RevealDeadline)` | 仅匹配承诺的揭示 |
| 终态 | `>= RevealDeadline` | 只读 |

到达 `RevealDeadline` 后，对未揭示的承诺按 `TimeoutPolicy` 处理（可配置）：

- **`PolicyExcludeUnrevealed`（默认，排除策略）**：未揭示的承诺被剔除并在结果
  `Excluded` 中以 `reason="unrevealed"` 记录；轮次携带全部有效揭示**正常完成**。
  这保证诚实揭示者不会因他人沉默而被无限期阻塞。
- **`PolicyAbort`（终止策略）**：只要存在任一未揭示承诺，整轮**终止**
  （`status=aborted`，不产出顺序，`Result()` 返回 `ErrRoundAborted`）；
  若所有提交都已揭示则正常完成。适用于"沉默即可能是攻击、宁可作废"的场景。

状态转换只由挂钟时间驱动。后台 ticker（`TickInterval`，默认 10ms）自动推进；
读取状态、提交、揭示时也会惰性推进，因此即使不等待 ticker也能观察到正确终态。

## 4. 持久化与崩溃恢复保证

`ServiceConfig.DataDir` 非空时使用 `<DataDir>/roundlog.jsonl` 持久化；为空时使用
纯内存实现（不保证崩溃恢复，适合测试）。

- 日志为**只追加 JSONL**，事件类型：`round`（创建）、`commit`、`reveal`、
  `result`（完成或终止）。
- **每条事件追加后立即 `fsync`**，然后才对调用方返回成功。因此"已确认"的提交、
  揭示、最终结果在进程崩溃/掉电后不丢失。
- 重启时**顺序重放**日志重建状态，天然幂等：
  - 按 `(round, sender, nonce)` 折叠重复的提交/揭示，恢复后重复提交仍被拒绝，
    **不会重复纳入交易**；
  - 已完成轮次恢复为 `finalized`，已确认结果原样可读，**结果不丢失、不改写**；
  - 已终止轮次恢复为 `aborted`，不会在重启后被错误地"补完成"；
  - 未走完的轮次恢复到崩溃时所处阶段，可继续提交/揭示，或由超时策略正常结束。
- 不同轮次以 `RoundID` 严格隔离，互不串扰。

崩溃窗口语义：若崩溃发生在事件 fsync 之后、应答到达客户端之前，客户端重试会得到
`ErrDuplicateCommit` / `ErrDuplicateReveal`——这正是"至多纳入一次"的预期行为。

## 5. 用法

```go
svc, err := fairorder.NewService(fairorder.ServiceConfig{
    DataDir:      "./data",          // 为空则内存模式
    TickInterval: 10 * time.Millisecond,
})
defer svc.Close()

cfg := fairorder.RoundConfig{
    ID:             "round-42",
    CommitDeadline: time.Now().Add(1 * time.Second),
    RevealDeadline: time.Now().Add(2 * time.Second),
    TimeoutPolicy:  fairorder.PolicyExcludeUnrevealed,
}
r, _ := svc.CreateRound(cfg)

// 提交阶段
salt := []byte("random-salt")
payload := []byte(`{"to":"bob","amount":100}`)
_ = r.SubmitCommit(fairorder.CommitRecord{
    Sender: "alice", Nonce: 1,
    Commitment: fairorder.MakeCommitment(salt, payload),
})

// ... 到达 CommitDeadline 后进入揭示阶段 ...
_ = r.SubmitReveal(fairorder.RevealRecord{
    Sender: "alice", Nonce: 1, Salt: salt, Payload: payload,
})

// 等待终态并读取已确认结果
status, _ := svc.WaitRoundFinalized(context.Background(), "round-42")
res, err := r.Result()
```

错误语义见 `types.go`：重复提交 `ErrDuplicateCommit`、重复揭示
`ErrDuplicateReveal`、揭示无对应承诺 `ErrCommitNotFound`、承诺不匹配
`ErrInvalidCommitment`、阶段未开放/已关闭 `ErrRevealNotOpen` /
`ErrCommitPhaseClosed` / `ErrRoundClosed`、终止 `ErrRoundAborted` 等。

## 6. 独立验证方法

最终结果可导出为 JSON（`fairorder.OrderResult`）。任意第三方无需服务即可验证：

```sh
go build -o orderverify ./cmd/orderverify
orderverify result.json
# OK: round=demo transactions=2 excluded=0   (退出码 0)
# INVALID: fairorder: order key mismatch ... (退出码 1)
```

校验内容：每个 `order_key` 是否等于 `SHA256(payload)`；条目是否严格按
`(order_key, commitment, sender, nonce)` 升序；身份是否唯一。
库内对应函数为 `fairorder.VerifyOrderResult`。承诺与盐值的对应关系可用
`fairorder.VerifyCommitment` 在揭示数据上另行复核。

## 7. 运行测试

```sh
go test -race -count=1 ./...
```

测试覆盖：

- 承诺生成/校验、长度前缀无歧义、排序键确定性（`crypto_test.go`）；
- 正常排序、阶段门控、重复提交/重复揭示、无承诺揭示、盐值或原文篡改被拒
  （`round_test.go`）；
- 两种超时策略（排除 / 终止）、全部揭示时终止策略仍完成、轮次隔离、配置边界、
  截止时刻左闭右开语义、创建时已过期轮次（`round_test.go`、`integration_test.go`）；
- **抢跑攻击**：复制他人承诺但不知盐值则无法有效揭示，被排除且不影响受害者顺序；
- **并发一致性**：多 goroutine 随机调度下多次运行产出字节一致的唯一顺序，
  并发重复提交恰好成功一次（`concurrency_test.go`，带 `-race`）；
- **崩溃恢复**：提交后 / 揭示后 / 终止后 / 完成后崩溃重启，使用真实临时目录与
  fsync 日志，验证不丢、不重、终态保持、多轮隔离（`recovery_test.go`）；
- 独立验证函数对篡改原文、乱序、重复身份的检测（`verify_test.go`）。

测试使用可注入的虚拟时钟（`ServiceConfig.Now`）确定性地驱动时间；另有真实时钟 +
后台 ticker 的端到端用例。

## 8. 范围与注意事项

- 本服务只负责**本地排序公平性**，不实现 P2P 广播、身份鉴权、加密通信或经济惩罚；
  参与方身份字符串 `sender` 与 `nonce` 由上层系统提供并保证其含义。
- 盐值必须由提交方使用密码学安全随机源生成且在揭示前保密；盐值复用或提前泄露会
  削弱提交内容的保密性（但承诺绑定与排序确定性仍然成立）。
- `PolicyExcludeUnrevealed` 下，故意沉默的攻击方只会让自己被排除，无法影响他人
  相对顺序；若业务要求"全员到齐否则作废"，请使用 `PolicyAbort`。
- 日志为只追加文件，长期运行的压缩/快照（checkpoint）不在当前范围内。
