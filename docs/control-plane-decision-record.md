# 运行中控制面：设计讨论决策记录

> 状态：定稿（2026-10-28）
> 性质：决策记录（rationale）。契约本文见 docs/CONTROL-PLANE-DESIGN.md v3 与 docs/DESIGN.md「运行中控制面」小节；本文回答"为什么是这些决策"，供未来修订时追溯。
> 覆盖：与 pi-mono 内核的对比结论、业界调研证据、D1–D10 裁决及其演变、评审拉锯中暴露的三个设计原则修正、实现批次。

## 1. 起点：一个边界问题

对照 pi-mono（badlogic/pi-mono）内核发现 go-agent 缺 steering 与回合间钩子。核实后的结论：

- pi 的 hooks/steering/QueueMode/finishTurn/prepareRequest **全部定义在内核层**（packages/agent/src/types.ts），不是 coding-agent 的实现细节；
- 业界四家（Claude Code hooks、OpenAI Agents SDK 的 RunHooks+max_turns、LangChain hooks-middleware、PydanticAI v2 Hooks）全部收敛在"内核定义类型化生命周期钩子"这一形态，均有官方文档佐证（2026-10-28 核实）；
- PydanticAI v2 的表述最具代表性："The inner loop of an agent is settled by now... The real leverage is in the layer around it"。

**结论**：go-agent 的"封闭内核 + 包装器"是设计选择而非缺陷，但它把"防 hook 破坏不变量"和"不提供 hook"绑在了一起，拒绝了比必要更宽的东西——存在一个中间地带：**限点位、强契约、有限能力的内核缝隙**不破坏单一执行路径的可推理性。go-agent 应取这个中间地带，而不是追平 pi 的 hook 面积。

## 2. 分层判据：什么进内核

讨论中收敛出两条判据，比任何具体功能都重要：

**判据一（执行时支持）**：需要循环内部执行时支持的能力进内核；构造期能表达的不进。
- 进内核：steering drain（包装器无法向持久化 history 注入消息）、回合末合作式终止（包装器无法提前干净收尾）、每轮配置读取。
- 不进：per-tenant 能力面隔离（per-user Agent 实例 + 共享 Store 的组合在构造期完成隔离，Agent 构造廉价、实例不可变天然并发安全）；模型/工具供应商级切换（Provider/Tool 包装器已覆盖）。

**判据二（状态归属）**：per-thread 的东西（历史、插话、配置选择）= 线程日志状态；per-tenant 的东西（Registry、审批策略、system prompt 基线）= 独立 Agent 实例。单实例多租户 + 运行时过滤是明确否决的方向。

## 3. 与 pi 的机制对照（核实后）

| 维度 | go-agent | pi |
|---|---|---|
| 循环 | 单层 for + maxIter 熔断 + FinishRound 决策（本设计） | 双层 while，无上限，防失控交给上层 |
| 插话 | durable 队列：input_queued/input_consumed 记录，drain 于回合顶部 | 内存双队列（steering/follow-up），abort 时退还编辑器 |
| 回合末决策 | FinishRound 钩子（default/continue/end 三值） | finishTurn 钩子（同构） |
| 请求前改参数 | prepareRequest 同位：config_changed 记录 + 每轮读配置 | prepareRequest 钩子（换 model/thinking/context） |
| 持久化 | 内核内置两阶段（declare/commit）+ 全部控制意图落日志 | 消费者订阅事件逐条写 JSONL，队列在内存 |

关键认知：pi 的插话只能"迟到不丢"靠不了队列——它的队列在内存里，abort 即退还。go-agent 的差异化不是"也能插话"，而是**插话和对话一样可恢复**：接受即持久化，崩溃后 Resume 时队列是日志里的事实。

## 4. 核心设计原则（讨论中三次修正的最终版）

**原则 A：接受即持久化（accept implies persisted），而非"持久化要趁早"。**
API 返回成功的时刻，消息必须已在日志里。注意这与"所有消息都该立即落库"不同：一条未被消费的插话是**队列成员而非 history 成员**，落库保护的是 API 的承诺（必达），不是消息的处理。由此推导出两种合法实现——接受时落库（方案 a），或干脆不接受（曾考虑的 v1 降级，被 owner 否决，理由见 D6）。

**原则 B：drain 点唯一且在回合边界。**
每轮固定顺序：读配置 → 压缩 → drain → 预算检查 → 请求。先压缩后注入（owner 提出）：压缩基于上一圈结束时的稳定 history 决策；新消息注入在压缩后天然免于被压掉；注入后的预算检查兜底残余溢出。

**原则 C：准入只拒物理不可行，容量不足走延后。**
准入（enqueue 时）拒绝序列化成本超过硬限制（canonicalCap/window）的输入；drain 时只消费"硬限制 − 当前 history"装得下的 FIFO 前缀，装不下的**延后**（留在队列，随下一轮压缩腾出空间重试）。绝不 force 注入、绝不因注满杀 run。已接受的输入可能迟到，但绝不会顶破硬限制。

**原则 D：护栏与决策分层。**
maxIter 数值熔断保留，优先级高于一切决策回调（terminal 圈的 Continue 视为默认调度）——护栏必须不依赖外部代码的正确性；FinishRound 是内容判定层，回答"应不应该停"，maxIter 回答"还能不能跑"。OpenAI Agents SDK 的 max_turns + hooks 并存是同构先例。

## 5. 决策记录（D1–D10 及修正案）

裁决时按"先阻塞性正确性、后产品取舍"分两组；全部采纳推荐或标注修正。

### 阻塞组

- **D6 外部写入协议 → (a) 乐观吸收**：runner 写点冲突时重读日志尾、吸收白名单控制记录后按真实 head 重试。讨论中曾提出 v1 降级（run 期间拒绝入队）与 runner 代写方案，均被否决——"运行中插话"是功能定义本身，durable 是差异化主张。吸收白名单（config_changed/input_queued，后续批扩 input_consumed 之外的新 kind 须显式登记）是长期纪律：漏登记 = 偶发冲突，须测试钉死"未知记录必须冲突"。
- **D7 FinishRound End 终态 → 新增 run_closed 记录**：重放层拒绝"含工具调用的非 truncated Done"，复用现有编码走不通；合成 assistant 违反"不写假消息"。run_closed 与 run_cancelled 对称（宿主策略收尾 vs 调用方放弃）。前置条件：三 adapter 探针验证"tool result 收尾 + 新 user 输入"的出站形状（已执行，全过；业界证据：PydanticAI 修复层、pi#8756、vercel/ai PR#8524——该形状是崩溃恢复的业界常态，解法在 adapter 层归一化）。
- **D8 无工具回合的 Continue → 钩子前移 + declare 泛化**：FinishRound 调用点移到终态判定之前（否则对"模型想停"的分支不可达，而那正是 Continue 的主用途）；无工具回合以 terminal_text 子状态持久宣告（重放层加白名单，非放松校验）。
- **D9 checkpoint 升版 → codec 3 + 双投影**：快照携带 effective_config 与 pending queue；v1/v2 快照遇新语义**无条件**回退全量重放——"版本标记不得让旧 reader 借旧快照绕过新语义"的既有裁决的直接延伸。
- **D10 幂等写入 → 调用方提供操作身份**：steering 的幂等键由调用方提供（同键跨重启确认，ErrInvalidRecord 时全量日志回查），config 的操作身份是"每次调用"（fresh 随机 ID，A→B→A 必须终态 A）。两种身份都是对的：steering 的操作身份 = 调用方意图，config 的操作身份 = 调用次数。**内容哈希不是操作身份**——内容相等不等于操作相等（评审第一轮 blocker）。

### 取舍组

- **D1**：匿名（无 store）run 不支持 steering（ErrNoStore）——durable 是价值主张，内存版是退化形态；FinishRound 纯内存回调，匿名可用。
- **D2**：QueueMode 固定 drain-all；one-at-a-time 留作运行时策略后加（记录格式不变）。
- **D3**：run 结束时残留队列由下一 run 首轮消费——follow-up 语义零新机制获得。
- **D4**：End 以工具结果收尾，不合成收尾消息（同"不写假 Done"）；依赖 D7 的 run_closed 编码与 D4 探针。
- **D5**：FinishRound 不得顺带改配置——决策钩子只决策，配置走日志，两条路径独立可推理。

### 评审拉锯产生的三个修正（原方案缺陷，均有确定性回归测试锁死）

1. **度量必须同单位**：准入曾用 raw rune、drain 用序列化 estimateRunes——转义文本（30 个换行 = raw 30/序列化 115）可穿透准入后永不排水。修正：准入与 force/drain 全部以序列化消息计量。
2. **护栏只能放宽、不能收紧**：曾把 provider 场景的预算护栏从 canonicalCap 收紧到 target，破坏了 nil-compactor 的既有契约（无压缩时大历史本应放行）。修正：护栏 = 旧基线与注入容忍取大，从不收紧。存量测试（nil_compactor、compactor_failures）是这条纪律的守门员。
3. **force 的隐藏假设**：force 消费最老一条隐含"压缩必达 target"——压缩禁用/失败时仍会触发预算错误杀 run。最终方案删除 force，改为**延后排水**：装不下就留在队列，绝不杀 run；代价是已接受的输入可能迟到（容量不足的诚实信号），而非崩溃。

另有一条评审流程教训：**修复写入必须逐步自校验落盘**。曾出现两次 eval 写入静默失败，导致评审者读到旧代码、浪费两轮复审；以及一次全局字符串替换误伤相邻测试。此后所有修改走自校验通道并先核实再送审。

## 6. 实现批次与状态

| 批次 | 内容 | 状态 |
|---|---|---|
| 探针 | 三 adapter 的 D4 形状验证（含存量 maxIter/settle 路径欠账） | ✅ 全过 |
| ① | config_changed + SetThreadConfig + 吸收协议 + checkpoint v3 | ✅ 评审 APPROVE |
| ② | input_queued/input_consumed + EnqueueInputWithID + D1 drain + 队列投影 | ✅ 评审 APPROVE（四轮） |
| ③ | FinishRound：run_closed + declare 泛化 terminal_text + 钩子前移 | 待实现 |

不变量承诺（全批次有效）：sealed 七种事件不增不减；TC-first、declare/commit 两阶段、maxIter terminal 冻结、提交点先落库后行动原样保留；非 steering 的预算超限仍报错（只有 steerering 注入享有延后容忍）。

## 7. 方法论备忘

- **先调研再设计**：D4 的形状问题业界早已付过学费（PydanticAI/vercel/pi 的文档、issue 与 PR 里躺着答案），第一版却直接设计了自研探针兜底。
- **评审要独立且带代码证据**：两批实现各经历 2–4 轮评审拉锯，blocker 全部是源级反例（不是风格意见），其中四个（D6 head 缓存、D10 内容 ID、checkpoint 重定基、度量不一致）是自查与测试都没抓到的正确性问题。
- **回归测试要写"反例区分度"**：ABA 用例的默认值必须与目标值不同（否则旧 bug 下假通过）；冲突注入器不得顺延 expected（否则绕过被测路径）。一个在 bug 存在时也能通过的回归测试不是回归测试。
