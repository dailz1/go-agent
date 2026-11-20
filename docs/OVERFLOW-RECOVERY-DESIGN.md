# go-agent 超长分类与反应式恢复设计

> 状态：v1（2026-11-20，issue #18）
> 契约级规则以 docs/DESIGN.md 为准：P1「Provider 超长分类与反应式恢复」及各处（issue #18，2026-11-20）修订。本文承载实现级细节：恢复预算推导、规范记录载荷、恢复状态机、配置来源证明、checkpoint 迁移与分类验收矩阵；两者冲突时以 DESIGN.md 为准并回改本文件。不修改 docs/EVENT-CONTRACT.md 等既有专项文档。

## 1. 恢复预算与进展判据

记号：W 为当轮有效窗口（轮内固定，恢复不改写）；T 为主动压缩目标 `targetRunesFor(W)`；H 为本次被拒请求的 canonical history 估算；Bprev 为本 episode 上次已尝试的恢复目标。

```
B1 = floor(3 * min(W, T, H) / 4)
Bk = floor(3 * min(W, T, H, Bprev) / 4), k > 1
```

- 实现用除法与余数计算，避免 `3*x` 溢出；B <= 0 直接停止。规则保证 B < W、B < H，第二次起还须 B < Bprev。
- 75% 与两个名额是首版政策值（2026-11-20 owner 裁决，issue #18 决3），不是厂商保证；第二次恢复的价值是容纳估算误差，代价是再丢弃一段历史、再付一次摘要与请求费用。首版不公开比例与次数配置，仅提供总开关。
- 后续名额必须重新计入已缩小的 H，不得对旧 B 直接乘 75%：W/T 不额外收紧时，H=18,000 → B1=13,500 → 合法结果 12,800 → B2 = floor(75% × min(12,800, 13,500)) = 9,600（而非 10,125）；该计算同样适用于普通故障后显式 Resume 的剩余名额。
- 恢复不加 carry 宽限，不静默削减 system、用户请求或输出参数，不另造更激进的压缩算法：使用已配置的 Compactor，内建保护组规则原样；自定义 Compactor 的既有强制校验只保证合法分组与 system 前缀，不虚称已获得内建策略的全部用户组保护。
- 采纳判据须全部成立：返回 error 为 nil、`Changed=true`、结果非空、可深拷贝、`validateCompacted` 通过、`Hafter <= B` 且 `Hafter < Hbefore`（`Changed=true` 但同大小同样失败）。
- 重发条件：新完整 outbound 的 `estimateRunes` 必须严格小于被拒 outbound 的保存估算（只保存数值基线，不保存 overlay 内容），防止新环境快照把省出的空间填回；callback 失败或 outbound 无进展不撤销已落库的合法压缩，但不发送该请求（blocked，reason=`no_progress`）。
- 摘要请求受 B 约束并使用本轮 Provider/options；摘要内部再报超长按本次压缩器失败处理，不递归启动恢复器。压缩链可按既有策略降级，但最终返回错误或不达 B 即停止（blocked），不像主动路径那样 warn 后重发已知失败请求。

## 2. 规范记录与载荷（修订：评审 F2）

新增 `store.SchemaV4=4`；两个 kind 的 schema 恰为 4（不得让 Schema=0 默认成 1），既有 kind 的 schema 不提升。两类记录只允许 active run、无 open declaration，且 RunID/round 匹配；`round` 指下一次完整助手声明所属的零基逻辑轮（无 open declaration 时 = lastRound+1）。`round`/`attempt` 等序号字段用 presence 校验区分缺字段与零值。未知 phase、attempt 倒退、run/round 不符、非法 history 均返回 `ErrIncompatibleLog`，不能当作普通日志跳过。

### 2.1 `context_recovery`

| 字段 | 含义 |
|---|---|
| `run_id` / `round` | 目标 run 与零基逻辑轮 |
| `episode_id` | episode 标识（绑定 run_id + round + 冻结 Provider/model） |
| `step` | 同一 episode 内本类记录的连续序号，从 0 起 |
| `provider` / `model` / `config_seq` | **原失败请求**的冻结身份与配置投影版本（记 o），与后续所选配置无关 |
| `phase` | pending / preparing / ready / dispatched / blocked / superseded（见 §3） |
| `attempt` | 本 episode 已占用的恢复名额计数 |
| `budget_runes` | 已保存的恢复目标 B（首次 pending 为 0） |
| `before_runes` / `rejected_outbound_runes` | 被拒时的历史估算 / 被拒请求的 outbound 基线 |
| `code` | 原始 overflow 分类码（如 `context_length_exceeded`、`1261`、`model_context_window_exceeded`） |
| `reason` | 状态迁移原因（如 `resume_after_provider_error`、`interrupted_attempt`、`no_progress`、`config_mismatch`） |

重放效果：更新当前 run 当前逻辑轮的恢复投影，不添加对话消息。superseded 形态额外携带 `selected_provider`、`selected_model`、`selected_config_seq`（记 s）与 `model_change_seq`（证明区间内最后一次实际改变 Provider/model 对的记录 Seq），三者语义不得合并进同一个 config_seq（见 §4）。

### 2.2 `history_replaced`

| 字段 | 含义 |
|---|---|
| `run_id` / `round` | 同上 |
| `operation_id` | 一次性操作标识，单独去重；同一次写重试逐字节同一内容，不因此增加名额 |
| `reason` | `proactive_compaction` / `overflow_compaction` / `legacy_anchor` |
| `history` | 完整新 canonical history |
| 仅 `overflow_compaction`：`episode_id` / `attempt` / `budget_runes` / `before_runes` / `after_runes` | 必须匹配 preparing 中的 episode/attempt/budget 并满足 §1 全部判据 |

重放效果：以完整 History 替换 canonical history；overflow 替换与转 ready 在同一条记录内原子生效。`proactive_compaction` / `legacy_anchor` 不得在活动 episode 中替换历史——须先完成 superseded（§4）或继续该 episode 的强制压缩；`legacy_anchor` 只用于把旧 v3 快照承载的压缩视图固定为规范起点，不要求变小，active/run/round/system/结构校验照常。

### 2.3 提交与写入规则

- 单条 `history_replaced` 就是提交单位："保存新历史"与"解除恢复状态"不得拆成必须同时成功的两条记录；JSONL 多记录 Append 不当作断电事务，不以批量 Append 代替该原子要求。
- Append 确认后才采纳 live history、交付 CompactionEvent 或发起下一次请求；可选 checkpoint 写失败沿用既有持久化错误出口，恢复以已确认的规范记录为准。
- 所有成功采纳的主动压缩经同一 `history_replaced` 写入口，再按需生成可选 checkpoint；只写 checkpoint 的压缩旁路废除（非持久路径仍为内存替换）。
- 稳定 ID 按 episode + 迁移 step 或一次性 operation_id 生成；同一次不确定重试字节完全一致。复用既有 ownership、expected head 与控制记录 rebase 协议；两类新 kind 是 runner 自写记录，不进入吸收白名单（rebase 只吸收 `config_changed`/`input_queued`），本轮请求快照不因 rebase 改换。

## 3. 恢复状态机（修订：评审 F3/F4/F5）

一个 episode 绑定 `run_id + round + 冻结 Provider/model`，上限两个恢复名额；每个名额最多一次 Compactor 调用，成功后进入一个有界主请求周期（沿用既有网络/429/5xx 重试与回退配置；名额限制的是"压缩后请求周期"，不是 HTTP 次数）。优先级固定：真实取消 → 主 Provider 来源的确认 overflow → 普通 retry/fallback；overflow 不走 Retryable、不睡指数退避，即使标记与 retryable 或 `ErrStreamingNotSupported` 被 join 也不先按普通错误重发。

零 chunk 与已交付 chunk 的分流（修订：评审 F4）：零 chunk 的已确认超长在同一调用内继续恢复（退出旧 iterator 并释放资源之后）；已交付任何非 nil `llm.Chunk` 后只保存 pending 并结束本次调用，不续流，由下一次显式 Resume 强制恢复。两条路径共用同一状态机与名额。

| 当前状态 / 动作 | 必须先保存的变化 | 崩溃或 Resume 行为 |
|---|---|---|
| 首次确认 overflow | `pending, attempt=0`，保存被拒请求估算与身份 | Resume 不检查普通触发线，直接处理 pending；不先 drain |
| pending 开始缩小 | `preparing, attempt=k`（加一），保存本次 B | 记录确认后才调 Compactor；preparing 残留 = 摘要是否运行未知，该名额不重用 |
| 压缩成功 | 一条 overflow `history_replaced`，同时转 ready | 使用已保存结果，不重跑该名额的摘要 |
| 准备重发 | callback/预算检查成功后写 `dispatched`，attempt 不变 | 写确认后才调用主 Provider；dispatched 残留不能原样重发相同历史 |
| 再次确认 overflow | `pending`，保留已占用 k 与最新失败估算 | k<2 才可 preparing(k+1)；k=2 则 blocked |
| 恢复周期被普通 Provider 错误终止（修订：评审 F5） | 保留 dispatched、attempt/B；追加 reason=`provider_error` 的结果备注并返回原错误 | 本次调用结束；下一次显式 Resume 才可用剩余名额进入更低预算，耗尽则 blocked |
| 无进展 / 无可用名额 / 压缩失败 | `blocked`，保存稳定 reason | 同配置 Resume 直接返回恢复错误，不请求、不再压缩 |
| 完整回复持久提交（修订：评审 F3） | 仅从尚无 provider_error 失败备注的 dispatched，经匹配当前 run/round 的 round_declared（含 terminal_text）或身份核验通过的普通 Done 清除 recovery（修订：评审 N2） | live 与 replay 投影执行同样校验；truncated Done 不能承担清除 |
| 显式取消结算 | 既有 run_cancelled 重放清除 recovery | 保持 cancelled 只读快照，不调用模型 |
| 显式 Provider/model 修复在 Resume 生效（修订：评审 F2） | `superseded`：保留原请求身份，另携带冻结新选择版本 s、selected pair 与 model_change_seq，按有限日志区间证明 | 旧 episode 退出；保留缩减结果；不要求冻结选择与 rebase 后最新配置相等 |

`step` 只计同一 episode 的 `context_recovery` 记录且从 0 连续增加；`dispatched → dispatched` 的单次结果备注（reason=`provider_error`）step 加一但 attempt/B 不变，它只记录该周期被普通错误终止，绝不授权再发请求，也不伪造新的 overflow。

### 3.1 清除规则（身份核验，修订：评审 F3/N2）

- 有活动 recovery 时，成功助手 declaration/普通 Done 只能从尚未携带 provider_error 失败备注的 dispatched 到达；要求 `recovery.run_id` = 当前 active run 且 `recovery.round` = 该成功回复的逻辑轮。带失败备注的 dispatched 之后仅允许：下一名额的 preparing、blocked、有合法证明的 superseded、终态结算（沿用既有终态兼容例外）；直接出现成功 declaration/Done 必须在 replay 中拒绝（本条修补 replay 规则缺口，不声称已证明合法 live writer 会生成该序列）。
- declaration 保留既有 RunID、连续 Round、消息与工具配对检查，匹配稳定 ID `<runID>-d<round>`；`terminal_text` 在这些检查与无工具调用校验完成后、重放分支的提前 `continue` **之前**清除 recovery，不得只在 switch 尾部统一清除；maxIter declaration 在对应身份校验后先清状态，再按原逻辑提交 skip/commit/Done。
- 普通 Done 必须 `Truncated=false`、稳定记录 ID 恰为 `<当前 runID>-done`、run active、无 open declaration、助手消息形态合法，且 `recovery.round` = `lastRound+1`；live writer 另用本次实际逻辑轮 `roundBase+i` 核对。Done 载荷没有独立 round 字段，由 run 身份、连续声明前缀的下一轮与 recovery 轮共同验证，不凭 ID 后缀或助手 role 推断；任一不符返回 `ErrIncompatibleLog`，不追加助手消息、不清状态。
- truncated Done 带活动 recovery 到达一律拒绝：合法路径已在匹配的 maxIter declaration 清除，不能让空消息的 truncated Done 越过该声明。
- live persistence/session 投影与 replay/threadView 投影执行同样的身份与状态检查：live 在 Append 确认后清除，replay 在同条规范记录的 fold 中清除；不能只补 reader 留下运行中旧 marker，也不能先清 live 再尝试落库。
- run_cancelled 按既有 RunID/稳定 ID 校验后可清任一恢复状态；run_closed 保留 RunID 与生命周期校验（合法合作终止的 declaration 已先清除 recovery）；新的 run_started 从空恢复状态开始。
- 兼容例外：legacy KindError 无身份载荷，沿用"关闭当前 active run"的旧语义并清除 recovery；它是失败终态兼容规则，不是成功回复，新恢复 writer 不得用它绕过身份检查。

### 3.2 崩溃与普通故障的保守处理

- 重放遇 preparing/dispatched 未确认完成状态，下一次 Resume 保守消耗剩余名额、按更低目标再压缩；已用完则 blocked。该分支可直接追加 preparing(k+1)，reason=`interrupted_attempt`，仍先保存再行动，不伪造新的远端 overflow；preparing 未产生历史替换时 H 仍为原历史但 B 必须低于上次 B，dispatched 已产生替换时缩减结果继续保留。
- （修订：评审 F5）普通 Provider 失败只结束本次调用，不写 permanent blocked：真实取消优先并立即退出（保留最后确认状态）；ctx 仍有效时先判已确认 overflow、再判普通 retry/fallback，不假设 joined 错误只匹配一种分类。既有普通有界 retry 耗尽后，保留原错误链与 RunInterruptedError 外层，记录 dispatched/provider_error；不得替换成"恢复预算耗尽"，也不用普通错误伪造 pending 或再次 overflow。本次不再调用 Compactor 或主模型。
- 下一次显式 Resume 有名额时：先保存 preparing(k+1)、reason=`resume_after_provider_error`，按 §1 从当前 H 与 Bprev 推导严格更低 B，再压缩并持久保存；不退回 ready 重用同一份历史发送，不重置名额。备注未确认落盘时当前调用仍停止，错误链同时保留原 Provider 与持久化错误；下次按实际 dispatched 日志的 `interrupted_attempt` 规则处理，不猜测是否已发送。两名额已耗尽时本次如实返回普通错误，下次 Resume 才按耗尽状态返回 blocked。
- 首次 callback 在 dispatch 前失败保留 ready；transport retry/fallback 的后续 callback 失败保留已确认 dispatched；两者均返回原 callback 错误，当前调用内不另起压缩。
- 代价声明：先占名额再行动可能在真正发出请求前崩溃而浪费名额，换来跨重启仍有上限；不承诺远端请求恰好执行一次，本地日志无法与远端推理请求原子提交。取消、consumer break 不自动写 run_cancelled，既有 SettleThread/SettlementToken 规则不变；取消发生在准备动作之前不启动动作，marker 未保存时不能保证下次知道这次超长，不得为隐藏该窗口用无限生命周期 context 后台写日志。

## 4. 配置来源证明（修订：评审 F2/N1）

live 恢复周期始终使用原轮的 Provider、有效 model key、options、tools、W；仅调整构造期 C、Compactor 或工具集合不自动清除 blocked。显式修复遵守五条证明规则：

1. **投影版本与变化来源分开。** `config_seq` 表示已折叠的最后一条任意 config_changed 的 Seq（无记录为 -1，Seq 0 合法且必须原样保存，序号字段用 presence 校验）；原请求冻结版本记 o，新选择冻结版本记 s，两者都可能指向只改 Thinking 的记录；`model_change_seq` 才标识证明区间中实际改变 Provider/model 对的来源。threadView、persistence、完整 replay、旧 snapshot 扫描、appendRebaseable、absorbTail 一致维护投影版本。
2. **显式修改须有可复演的来源。** 读取规范日志有限区间 `o < Seq <= s`，从旧请求保存的 pair 出发按序只应用显式给出的 Provider/model 字段（Thinking 不改变该对）；必须存在至少一次实际改变该对的记录，最终 pair 与旧 pair 不同且等于本次选定 pair；`model_change_seq` = 区间内最后一次实际改变该对的记录 Seq，可小于 s（o=10 的 A、Seq 20 切 B、Seq 21 改 Thinking → s=21、model_change_seq=20）。只改 Thinking、同值重复设置、A→B→A 后仍选 A，均不能恢复名额。
3. **校验冻结选择，不追最新值。** superseded 及对应请求使用同一次选择冻结的 pair/options/W/s；写入 rebase 吸收更晚配置时不修改该记录的证据或请求选择（选择冻结于 Seq 20 为 B、写入时 Seq 21 已切 C，仍按截止 s=20 验证 B，C 留给下一入口）；replay 也不要求 B 等于当时最新投影。superseded 不回滚当前配置投影，只结束被证明已替代的旧 episode。
4. **checkpoint 不能藏掉证明。** v4 checkpoint 保存当前 config_seq 与 recovery 中的原请求 pair/o；恢复入口仍经 `Store.History` 回查有限区间计算 s 的选择证明，处理已写入的 superseded 时使用记录内的 selected pair/s/model_change_seq，由生产 fold 调用者提供相同区间；不得只看快照中最终 pair 和最后 Seq 就批准。区间缺失、版本不支持或来源不匹配返回兼容性错误；checkpoint 路径与 full-log 路径一致。
5. **重建 Agent 的默认漂移不构成显式修复。** 本次有效 pair 与旧请求不同但区间推导不能证明完全由显式字段修改得到时，返回 `ContextRecoveryError{Reason:"config_mismatch"}`，保留 recovery，不请求、不 supersede、不占名额；宿主需显式 SetThreadConfig 修复或结算旧 run。证明从旧请求冻结 pair 出发，不用新 Agent 构造默认值补出变化；model key 是内核可见的 llm options Model（允许为空），相同空 key 背后的 adapter 私有默认漂移不可观测，也不能据此自动清状态。

（修订：评审 N1）证明上下文覆盖所有生产 replayRecords 调用者，含 replayThread 与 `agent/settle.go` 的 confirmSettlement：confirmSettlement 已持有完整日志，以原 token 的 `ExpectedHead` 为边界从 `records[:h]` 构造同一证明上下文，供前缀 fold 与随后 `records[h:h+1]` 的取消确认 fold 使用；每个 superseded 仍只按其冻结版本验证。不得跳过证明、不借 h 之后的记录或最新配置补证，保证原 token 重试执行相同的 superseded 校验。

满足证明时：先持久保存 superseded、保留缩减后的 history，再用本次冻结选择进入正常预算和请求；新模型再超长则建立新 episode。同配置 blocked 后的人工出口：显式 SettleThread 放弃原 run 后新开 run，或显式切换 Provider/model 后 Resume；新增输入与 steering 不构成重置，本期不新增"清零次数"公共 API。Resume 重新计算 W；ready 必须同时满足保存预算与新 W/target，进一步压缩需剩余名额；W 变大不自动取消标记，nil Compactor 的 ready 处理见 DESIGN.md 退化路径表（修订：评审 F6）。

## 5. checkpoint 版本与迁移

1. 新 checkpoint 使用 schema=4、codec_version=4，新增 nullable `recovery` 投影（phase、attempt、B、episode、原请求冻结 Provider/model/config_seq、最后被拒请求估算），并单独保存当前配置投影及其 config_seq；config_seq 是最后一条已折叠 config_changed 的 Seq，不是模型切换来源；无恢复状态必须显式可区分于旧格式缺字段。superseded 的证明选择回查规范日志（`o < Seq <= s` 有限区间），不在快照中复制整段配置变更历史；纯 replay fold 所需的有限证明区间必须由所有生产调用者提供（含 replayThread 与 confirmSettlement），使用 checkpoint 与从头回放须得到同一结论。（修订：评审 F2/N1）
2. 新 reader 继续支持 schema 1-3 记录；没有恢复记录就没有 force 状态。旧日志不推测历史错误正文，不凭 KindError 反推超长；KindError 的原终态语义不变。
3. 结构有效的旧 v3 checkpoint：先经 History 检查其覆盖前缀不含 schema=4 记录才可按原语义采用，并从该前缀补出旧快照缺少的 config_seq（初值 -1，Seq 0 的 config_changed 更新为 0）；前缀含 schema=4 记录必须全日志重放，不得让旧 snapshot 隐藏恢复标记；发现未来版本报兼容性错误。兼容扫描与全量回退均只采用 Latest 所见 `Seq < Head` 前缀，之后的新控制写入留给吸收协议；superseded 的专用证明回查进一步截止于所选版本 s。v1/v2 沿用既有全日志回退规则。（修订：评审 F2）
4. 采用 legacy v3 checkpoint 的压缩视图后，第一次写恢复状态或 v4 checkpoint 前，先用一条 `history_replaced(reason=legacy_anchor)` 固定当前完整 seeded history（不要求变小，active/run/round/system/结构照常校验）；若第一次写入本身就是主动压缩的 history_replaced，已完成该作用，不重复写 anchor。
5. 全日志回放时 legacy_anchor 直接建立历史视图，随后所有新压缩与恢复都可重建；旧日志从未保存到规范记录的摘要无法凭空还原——旧 checkpoint 已损坏时维持现有从原始记录重建的行为，不声称还原旧摘要。
6. 支持但损坏的 v4 checkpoint 回退 History；未知未来 schema/codec 返回兼容性错误，不用回退掩盖。新 writer 不再写 v3 checkpoint；旧 agent 读 schema=4 或 codec=4 必须失败，不支持向旧二进制降级写入。
7. 同步更新 Store 的 kind/schema 验证、JSONL 与 Memory 共用校验、agent codec，以及 `store/jsonl_file.go` 独立的 `Schema > SchemaV3` 读取拒绝条件（修订：评审 F2）——仅改 store.go Append 校验可能让 Memory 测试通过而 schema=4 线程磁盘重开失败。新 kind schema 恰为 4；新旧版本读取约束同时覆盖 Append 与 load；Store 方法签名、fsync、Seq、幂等规则不改。

可丢弃 checkpoint 的保证精确限定为：后续规范记录已提交的历史替换、恢复限制与配置证明，不依赖 checkpoint 重建其结果；尚未提交 legacy_anchor 的旧摘要仍依赖原 snapshot，删除该 snapshot 不能要求还原当时摘要，不得对外省略该边界。

### 已知权衡

v4 checkpoint 在采纳前会重放其覆盖的规范日志前缀，并逐项核对 history、system、config、queue、run 生命周期、recovery 与 config_seq。任一项不一致就丢弃快照，完整重放日志。因此，当前重新打开线程的成本与 checkpoint 覆盖的前缀长度成正比，不能依靠定期压缩将重开成本限制为快照加尾部日志。后续有界锚点设计落地前保留这一正确性优先的权衡；本次修复不引入新的锚点机制。

## 6. 分类入口与验收矩阵

### 6.1 负例验收矩阵（adapter 层 fixture + scripted Provider）

每行做 adapter 层 fixture 测试并接 scripted Provider；正例按相应边界进入恢复，负例确认不触发恢复压缩或重发。HTTP 与 SSE 不互相代替。

| 输入 | 应标记超长 | 验收点 |
|---|---|---|
| OpenAI HTTP 400，error.code 精确 `context_length_exceeded` | 是 | 保留原 APIError，可 errors.As；Retryable 仍为 false |
| Responses/Codex failed.error.code 精确命中，零 chunk | 是 | 可进入当前调用内恢复 |
| GLM HTTP 400 code 1261；GLM finish_reason 精确命中 | 是 | 两种通道各自覆盖，不能依赖合成 Body 文字 |
| 同一肯定信号，但先前已有 text/reasoning/tool/Done chunk | 是 | 保存 pending，结束流；当前流不再请求 |
| `error.code=max_tokens_exceeded`，message 恰含 context_length_exceeded 字样 | 否 | code 不得被 message 子串覆盖 |
| Responses incomplete.reason=max_output_tokens / content_filter | 否 | 保持原 incomplete 错误 |
| GLM / Chat Completions finish_reason=length，含零输出变体 | 否 | 保持原映射，禁止把"没有输出"当超长证明 |
| 400 `max_tokens must be less than ...`；max_output_tokens 非法 | 否 | 不通过删除历史掩盖参数错误 |
| 429 rate_limit_exceeded，message 为 too many tokens | 否 | 仅现有网络/限流重试可发生 |
| 429 insufficient_quota、Codex usage_limit_reached | 否 | 保持各 adapter 原有 quota 与 NonRetryable 规则 |
| 500、401、403，body 恰包含 context_length_exceeded | 否 | HTTP 状态与已核验 code 组合不符 |
| 413 空体 / HTML / payload too large / image too large | 否 | 请求字节大小不是本题已验证的上下文信号 |
| 400 invalid_request，无 code 或 code=null | 否 | 不泛化所有 400 |
| malformed JSON、未知 code、普通 Go error 带相同字串 | 否 | 不在内核扫描 Error() |
| callback、工具或 compactor 自己返回 overflow 标记 | 否，不触发本轮恢复器 | 只有主请求的直接 Provider 错误和 iterator 错误进入恢复分类 |
| 真实 ctx 已取消，同时收到标记 | 分类可保留，禁止自动执行 | 不压缩、不再发请求；取消可通过 errors.Is 识别 |
| 正常成功，但 usage 超过声明 W | 否 | 静默截断 / usage 校准不属于本次可核验拒绝恢复 |

### 6.2 组合回归 C1-C8（修订：评审 F4/F5）

| 组合 | 输入及通道隔离 | 必须看到的结果 |
|---|---|---|
| C1 | Code 为空、缺失或 null，Type 恰为 context_length_exceeded | 不标记；Codex 的 Code→Type fallback 不得参与 overflow 判定 |
| C2 | Code 是 max_tokens_exceeded / max_output_tokens，Type 或 Message 含 context_length_exceeded | 不标记、不恢复；输出限制 code 不被旁边字段覆盖 |
| C3 | 分别构造 Responses HTTP 非 2xx `error.code`、非流式失败 `response.error.code`、SSE failed 的 `response.error.code`、顶层 error 事件自身 `code`；再把肯定信号只放进该通道不适用的位置 | 只读取该通道许可的字段位置；各自正例可标记，错位字段负例不命中；四个入口不能只测其一 |
| C4 | response.incomplete 的 reason=max_output_tokens，同时在 code、type、message 或嵌套 error 放 context 字样/干扰 code | 按事件类型先排除该通道，保持 incomplete 错误，不落入 failed/error 共用分类后误触发 |
| C5 | GLM 同一 SSE payload 同时包含文本和 overflow finish_reason；分别测试之前零 chunk 与之前已交付 text chunk | 当前解析器会丢弃该 payload 尚未交付的 chunks：前者内核仍为零 chunk，退出旧 iterator 后可恢复；后者 started 已为 true，只存 pending 并结束当前调用。不得从原始 payload 文字反推已交付 |
| C6 | 同一次返回非 nil chunk+error；typed-nil chunk 指针+error；包装/未知 chunk+error | 三者都在检查 error 前通过接口 `chunk != nil` 设置 started；不得依赖已知类型 switch 或 accumulator 文本；本次不自动续发 |
| C7 | `%w` 包装 overflow 标记；标记与 retryable APIError / `ErrStreamingNotSupported` join；真实 ctx 取消；callback/Compactor 返回同样标记 | 主 Provider 来源仍可识别；取消优先，其次 overflow，再普通 retry/fallback。callback/Compactor 来源不触发本轮恢复；errors.Is/As 保留原链 |
| C8 | 单许可 Provider 流包装器：旧 iterator 返回零 chunk overflow，摘要或下一 ChatStream 也需取得该许可 | 退出 range、旧 iterator 与包装器 defer 完成后，才持久 marker、调用摘要或下次请求；先观察到释放，再观察到新获取，无互相等待；清理期间取消不启动后续工作 |

### 6.3 逐入口解析与实施文件范围

- Responses 四入口分开解析与测试：HTTP 非 2xx `error.code`、非流式失败 envelope 的 error.code、SSE `response.failed` 的 `response.error.code`、SSE 顶层 error 事件自身 code（顶层 streamEvent 现缺 Code/Param 字段，单独扩展）；肯定信号只出现在该通道不适用位置时不得命中（C3）。
- Codex：`classifyServerError` 的 Code→Type fallback 不参与 overflow 判定，只检查原始 server.Code，不从包装后 codex.Error.Code 反推；failed/incomplete/error 共用分支之前先确定事件类型与许可 envelope，incomplete 即使带 context 干扰字段也不得借共用入口变成 overflow（C4）；HTTP 要求原始 Code 与真实 HTTP 400 同时匹配。Codex HTTP 错误分类的适用范围是本议题的 400 分支，其他 HTTP 状态保留原错误链与 Retryable 行为。
- GLM：HTTP 通道解析 `error.code == "1261"` 且 HTTP 400（整数码有界兼容解析，不把任意含 1261 的字符串当匹配）；SSE/finish_reason 在已解析分支直接标记 `model_context_window_exceeded`，`length`/`network_error` 不标记。
- openai Chat Completions：HTTP 400 + `error.code` 精确 `context_length_exceeded`；未取得该端点在线错误样本前不得把其他 OpenAI 产品的枚举声称为该端点完整错误码表。
- 实施文件：`llm/errors.go`（哨兵与 wrapper）、四个适配器错误解析、`agent/provider_call.go`、主循环与压缩持久化入口、`agent/persist*.go`、`agent/config.go`（配置版本/来源证明）、`store/store.go` 共用 Append 校验、`store/jsonl_file.go` 独立 load 校验与磁盘重开测试、`agent/settle.go` confirmSettlement 证明上下文（修订：评审 N1）。只用标准库；不接 harness；不引入第三方依赖；不新增公共 channel；不拆出第二条同步执行路径。

## 7. 发布证据边界

已核验证据：官方错误码文档、官方 SDK/源码固定提交、pi/opencode 当前源码、Letta Code PR #3489（closed 未合并，维护者确认 output-limit 泛化匹配层次错误，支持 D1 适配器分类与输出额度负例）。未采集真实厂商在线超长正例与相邻负例：对相应厂商宣告支持前，须采集脱敏真实样本（status、完整脱敏错误 envelope、请求输出预算、可得 usage、是否已交付 chunk），不得为通过验收临时放宽匹配，不用 fixtures 代替在线覆盖声明（issue #18 决5）。GLM 1261 的数字形态是否在线出现仍待验证。
