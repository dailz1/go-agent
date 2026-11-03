# 20 · 消息与事件模型（llm 包）

> 内核文档集第 20 篇。本文只讲 `llm` 包的**数据模型**：`Provider` 接口、`Message`/`ContentBlock` 内容块模型、`Chunk` 流式联合类型、`Usage`、`APIError` 叶子错误，以及四个适配器（openai / glm / openairesponses / codex）在数据形状上的差异。
> HTTP 请求管线、SSE 扫描、有界 I/O（10MB body / 1MB 行 / 120s 客户端超时）与重试调度属传输层，另见第 21 篇（传输与重试管线），本文只在接口交界处提到它们。事件层的七种密封 `AgentEvent` 与"为什么 TC-first"已在第 10 篇（自主循环）§3 讲透，本文只讲 Chunk→Event 的映射边界，不重复其论证。
> 行号锚点基于 2026-10-29 工作区（基线 65fea54，含 provider 热换改动）；接口契约以 docs/DESIGN.md 为准，两者冲突时以 DESIGN.md 为准。

## 1. Provider 接口：一个故意不密封的接缝

### 1.1 接口形状

```go
// llm/provider.go:19
type Provider interface {
    Name() string
    Chat(ctx context.Context, messages []Message, tools []tool.ToolInfo, opts ...Option) (*Message, *Usage, error)
    ChatStream(ctx context.Context, messages []Message, tools []tool.ToolInfo, opts ...Option) (iter.Seq2[Chunk, error], error)
}
```

三个方法各管一件事：`Name` 给出热切换解析用的身份字符串（`"openai"`、`"glm"`、`"openai_responses"`、`"codex"`——`SetThreadConfig` 按 `Name` fail-closed 匹配 provider，未知名报 `ErrUnknownProvider`，见第 14 篇控制面）；`Chat` 是一次性请求；`ChatStream` 返回惰性迭代器（llm/provider.go:26-30 的注释明确 "lazy — no network activity occurs until the caller begins ranging"，早退安全）。流式不支持的 provider 返回 `ErrStreamingNotSupported` 哨兵（llm/provider.go:11），调用方用 `errors.Is` 识别后回退 `Chat`——agent 循环的回退路径把这个回退结果重新合成为 chunk 流（§3.5），所以哨兵必须一路透传，包装器守则明令不得吞掉它（docs/DESIGN.md:62）。

### 1.2 为什么 Provider 不密封、数据类型密封

`llm` 包里有一组刻意的密封/开放分工：

| 类型 | 密封？ | 原因 |
|---|---|---|
| `ContentBlock`（llm/message.go:29） | 密封（未导出 `blockType() string`） | 变体集合是内核契约的一部分：agent 累积器、store 持久化、agenttest 录制回放都要**穷尽地**类型切换它们 |
| `Chunk`（llm/chunk.go:20） | 密封（未导出 `chunkType() string`） | 同上：流协议事实集合由本包定义，外部无法新增变体 |
| `Provider` | **不密封** | 它是扩展点。P2-1 的 Provider 中间件（包装器模式）要求外部包能实现该接口（docs/DESIGN.md P2-1，示例 `examples/middleware`）；密封会把观察/限流包装器挡在门外 |
| `Option` / `Options` | 不密封 | 选项是数据不是行为 |

密封的收益与 agent 事件层同理（第 10 篇 §3.5 已论证）：新增变体从兼容性问题变成编译问题——消费者 `switch` 少一个分支，编译器或显式 `default` 报错路径立即暴露；变体演化只发生在本包内，由 DESIGN.md 先行（docs-first 约定）。type switch 必须同时处理值与指针两种形态（项目约定；深拷贝历史会产生 `*ReasoningItemBlock` 指针形态，适配器转换代码里随处可见 `case llm.ReasoningItemBlock, *llm.ReasoningItemBlock` 的双分支）。

### 1.3 Options：公共子集 + 透传策略

`Options`（llm/provider.go:35）只放**四个厂商都至少能表达或能安全忽略**的请求参数：`Model`、`MaxTokens`、`Temperature`、`Stop`，加两个透传开关 `Thinking`（字符串 "enabled"/"disabled"，适配器不支持则忽略——openairesponses 即忽略）与 `JSONMode`（走 `response_format`）。`ApplyOptions`（llm/provider.go:77）折叠函数型选项。

厂商特有的参数**不进**这个结构：GLM 的 `do_sample`/`tool_stream`/`request_id` 是 `glm.ProviderOption` 构造期选项；openai 的 `stream_options.include_usage` 是 adapter 内部 wire 细节（llm/openai/adapter.go:181）。这条分界线是四层选项体系（agent `Option`、per-call `RunOption`、`llm.Option`、各 adapter `ProviderOption`）的一部分：**调用语义的参数走 `llm.Option`（每次调用可变），接入语义的参数走 adapter 构造选项（构造期绑定）**。例外只有一个——glm `WithTopP` 在返回的闭包里就地夹取 [0.01, 1.0]（llm/glm/adapter.go:79-86），是项目里唯一"在选项函数内做值归一"的地方（其余归一化全部在构造函数里）。

### 1.4 厂商特有字段的三条去向

跨厂商差异的数据字段，在本包按三条规则归位：

1. **进入公共模型、成为密封变体或字段**——当且仅当它必须穿越 history。理由：history（`[]Message`）会被 store 持久化、跨厂商热切换回放、agenttest 逐字节录制，所以任何"必须保真进入历史"的东西都必须是公共类型。两个例子：`ReasoningItemBlock`（Responses 的条目级加密推理项，llm/message.go:74）与 `ToolUseBlock.Input` 的 `json.RawMessage`（保参数原文，不二次解析）。
2. **留在 adapter 的 wire 类型里**——纯请求/响应形状。OpenAI/GLM 的 `chatMessage`、Responses 的 `inputItem` 都是包内私有结构；`reasoning_content` 字段只在出站转换时与 `ReasoningBlock` 互转，模型本身不知道这个字段。
3. **归入 `Usage`**（llm/usage.go:6）——计费语义公共化：`InputTokens`/`OutputTokens`/`ReasoningTokens` 三个 int64 字段，配 `Add`/`Total`/`IsZero` 算术。GLM 从不返回 reasoning tokens（恒 0，llm/glm/adapter.go convertUsage 注释），OpenAI 从 `completion_tokens_details.reasoning_tokens` 填入，Responses 从 `output_tokens_details` 填入——差异在各自 `convertUsage` 里消化，公共模型不感知。

## 2. Message 与内容块模型

### 2.1 一切内容皆块

```go
// llm/message.go:22
type Message struct {
    Role    Role           `json:"role"`
    Content []ContentBlock `json:"content"`
}
```

`Message` 只有角色和有序块列表，**没有** content 字符串字段。这是跨厂商统一的根：OpenAI Chat Completions 的 `content` 可以是字符串或分块数组、Responses 是 typed items、GLM 同 CC——如果公共模型带一个 `Text string` 字段，每家适配器都会出现"text 到底放哪"的歧义。统一为块列表后，适配器在出站时自行决定压扁形态（OpenAI/GLM 把单 TextBlock 发回纯字符串以省 token，llm/openai/adapter.go:331 的注释 "Single TextBlock is sent as a plain string for efficiency"；多块或含图时发数组）。

OpenAI 的字符串形态在**入站反序列化**时兼容：`Message.UnmarshalJSON`（llm/message.go:147）接受 content 为字符串、数组或 null 三种 JSON 形态——字符串直接变一个 `TextBlock`，null/缺失变空切片。这让调用方喂入 OpenAI 原生格式的消息（或 agenttest 回放录制）无需预处理。

四种 `Role`（system/user/assistant/tool）与业界四方对齐；tool result 不是独立的 role 消息内容，而是 `RoleTool` 消息里携带 `ToolResultBlock`。

### 2.2 六种内容块与不变量

| 块 | 定义 | 关键不变量 |
|---|---|---|
| `TextBlock` | llm/message.go:34 | 纯文本；多条 TextBlock 在 CC 适配器出站时以 `\n` 连接（llm/openai/adapter.go convertAssistant） |
| `ReasoningBlock` | llm/message.go:42 | CC 协议**内嵌**推理文本（DeepSeek Reasoner / GLM thinking 风格）；与 `ReasoningItemBlock` 语义不同，见 §2.4 |
| `ImageBlock` | llm/message.go:50 | URL 或 `MIMEType + []byte` 内联二选一；出站时内联形态由适配器编成 `data:` base64 URL |
| `ToolUseBlock` | llm/message.go:60 | 模型**请求**调用工具：`ID` + `Name` + `Input`（`json.RawMessage`，保参数原文不解释）。ID 是与 tool result 配对的键 |
| `ReasoningItemBlock` | llm/message.go:74 | Responses **条目级**推理项：服务器侧 `ID` + 不透明 `EncryptedContent` + `Summary` 文本。协议要求回放时置于其前导的 function_call 之前、保持声明顺序（docs/DESIGN.md:260）；CC 适配器跳过它，不产出也不出错 |
| `ToolResultBlock` | llm/message.go:84 | `ToolUseID` 指回配对的 `ToolUseBlock.ID`；`IsError` 镜像 `tool.ToolResult.IsError()`——工具**软失败**回灌 LLM 的通道，绝不上升为 Go error（软/硬错误分界是 tool 契约，见第 11 篇） |

块间不变量由构造器兜底：`SystemMessage`/`UserMessage`/`AssistantMessage`（llm/message.go:93 起）负责把 `Type` 字段填对；`AssistantToolCallMessage` 批量补 `"tool_use"`；`ToolResultMessage`（llm/message.go:126）把 `*tool.ToolResult` 变成 `RoleTool` 消息，**nil result 自动降为一条 error result**（"tool returned nil result"），避免空指针泄漏到协议层。llm/AGENTS.md 的约定：构造历史用构造器，不要手拼结构体字面量——`Type` 字段填错会在跨适配器转换时静默走丢分支。

### 2.3 反序列化为什么对未知块类型报错

`unmarshalContentBlock`（llm/message.go:192）按 `type` 判别式分发六种块，未知类型返回错误（llm/message.go:220）。这与 chunk 层"未知类型静默忽略"（§3.4）形成有意的对比：

- **加载侧（UnmarshalJSON）严格**：消息历史是持久化的契约数据，静默丢块等于让 store 回放悄悄产出残缺历史——错误必须显式暴露。
- **流式侧（chunk switch）宽容**：chunk 面对的是**活的协议演化**，服务器明天发一种新字段，旧内核应该继续工作而不是中断长任务。

同一个"演化的容忍度"问题，两处答案相反，因为输入的信任级别不同。

### 2.4 两种 reasoning 块：为什么并存

`ReasoningBlock`（内嵌文本）与 `ReasoningItemBlock`（条目级加密项）并存是 2026-09-11 的裁决 D3（docs/DESIGN.md:252-254）：当时的问题是 Responses 协议的推理项**没有** CC 形态的等价物——它是服务器侧有 ID 的独立条目，回放靠 `encrypted_content` 原样透传，而 `ReasoningBlock` 只是消息里的一段文本。把两者塞进一个带可选字段的块（rejected alternative）会让"哪些字段在此形态下必须为空"变成运行时约定；分成两个变体让非法状态不可表示，类型切换即穷尽。

跨厂商携带的后果由**适配器出站转换层**吸收，内核 loop/store/重放零参与——这是 DESIGN.md 冻结的降级契约表（docs/DESIGN.md:236）：

| adapter | `ReasoningBlock`（CC 内嵌）出站 | `ReasoningItemBlock`（条目级）出站 |
|---|---|---|
| openai | 透传为 `reasoning_content`（DeepSeek thinking 需要）llm/openai/adapter.go:409 | 跳过（协议无对应） |
| glm | strip（服务器不接受）llm/glm/adapter.go:456/:483 | strip |
| openairesponses | 丢弃（不属于 item 模型） | 原样回放（ID + 加密内容，保序，置于 function_call 之前） |
| codex | 跳过 | 原样回放；但对**缺 ID 的自有条目**硬错误（capability error，见 §5.5） |

规则一句话：任何适配器不因外来推理块硬错误（codex 对自身产出条目的严格性除外）。该契约由每个适配器的 `cross_provider_degrade_test.go` 与 `agent/provider_hotswap_test.go` 钉死——这是 provider 热切换（第 14 篇）能成立的数据层前提。

## 3. Chunk 流模型与 agent 事件的边界

### 3.1 六种 chunk：协议事实的最小词汇

```go
// llm/chunk.go:20
type Chunk interface { chunkType() string }
```

| Chunk | 携带 | 累积语义 |
|---|---|---|
| `TextDeltaChunk`（:26） | `Text` 增量 + `OutputIndex` | 消费者拼接；`OutputIndex` 标记 Responses 输出数组中的条目位置，CC 适配器恒为 0 |
| `ReasoningDeltaChunk`（:41） | 推理**文本**增量 | 可读文本（reasoning summary delta），区别于不可读的加密条目 |
| `ToolCallStartChunk`（:51） | `Index` + `ID` + `Name` | 每次调用发一次，开一个累积器 |
| `ToolCallArgsChunk`（:66） | `Index` + `ID`（可空）+ `Delta` | 参数 JSON 的**字符串碎片**，按 Index 追加拼接 |
| `ReasoningItemChunk`（:82） | `OutputIndex` + 完整 `ReasoningItemBlock` | 整项一次性交付，非增量 |
| `DoneChunk`（:93） | `FinishReason` + `*Usage` | 终结信号，此后无 chunk |

设计要点是**键的选型**：CC 协议的 tool_calls 用 `index` 定位（OpenAI 的 `tool_calls[].index`），Responses 用 `output_index` 统一索引所有条目类。公共模型把两个键都收进来——`ToolCallStartChunk.Index` 是 int（CC 语义），`TextDeltaChunk`/`ReasoningItemChunk` 的 `OutputIndex` 是 int64（Responses 语义）。为什么不统一成一个？因为 CC 的 index 只在"本条回复的 tool_calls 数组"内有意义，Responses 的 output_index 跨条目类排序（reasoning、message、function_call 共享一个输出数组）；强行统一会让 CC 适配器伪造一个它没有的排序域。`TextDeltaChunk.OutputIndex` 是后来为 Responses 加的字段（docs/DESIGN.md:261），CC 适配器恒置 0，不破坏旧语义。

### 3.2 DoneChunk 的终结契约

三个子契约：

1. **流必须以 DoneChunk 结束**。openai/glm 适配器在流扫完而没有 Done 时主动报错（"stream ended without completion"，llm/openai/adapter.go:243-244）；codex 的 `scanResponse` 对缺失终态报 protocol 错误（llm/codex/stream.go:13 末尾分支）。agent 循环同样把"无 Done 的流"当错误（第 10 篇 fold 契约）。原因：DoneChunk 承载 FinishReason 与 Usage，静默缺失会让调用方把半截回复当完整回复。
2. **Usage 的 nil/零区别有语义**（llm/chunk.go:98-99 注释）：nil = 无 usage 数据（GLM 流式），非 nil 零值 = 服务器报告了但全为 0。累积器只记录最后一个非 nil Usage（agent/accumulator.go:80-82）；这个区别让"没统计"与"统计为零"在计费层可区分。
3. **Done 不等于 agent 终局**。chunk 层的 Done 只说"这条流完了"；`DoneEvent` 是循环层的调度决策（无工具调用 / maxIter / FinishRound），两者相隔整个工具执行段。把这两层终局混为一谈是流式 agent 的经典错误。

GLM 的特例：它的流式终包可能只带 usage 不带 choices（`len(resp.Choices)==0 && resp.Usage != nil`），适配器为其合成纯 usage 的 DoneChunk（llm/glm/adapter.go parseStreamPayload 对应分支）；OpenAI 则是显式请求 `stream_options.include_usage`（llm/openai/adapter.go:181）让终包带 usage。

### 3.3 Chunk → AgentEvent 的映射边界

映射表（发射点在 agent 流式循环，agent/agent.go:492-518；事件定义 agent/event.go）：

| llm Chunk | agent Event | 边界处理 |
|---|---|---|
| `TextDeltaChunk` | `TextDeltaEvent`（agent/agent.go:497-499） | 1:1 透传，yield 后仍喂累积器 |
| `ReasoningDeltaChunk` | `ThinkingDeltaEvent`（agent/agent.go:492-495） | 1:1 透传——是**可读文本**，UI 有展示价值 |
| `ToolCallStartChunk` / `ToolCallArgsChunk` | **无事件**，只进累积器（agent/agent.go:502-507） | 整流装配完成后按声明顺序发 `ToolCallEvent` 批（TC-first，第 10 篇 §3.2） |
| `ReasoningItemChunk` | **无事件**（agent/agent.go:508-512，注释 "not a consumer event"） | 不透明载荷，仅按 `OutputIndex` 参与保序装配；消费者从 `DoneEvent.History` 拿权威快照（第 10 篇 §3.4 已论证） |
| `DoneChunk` | **无事件**；Usage 计入 `totalUsage`（agent/agent.go:519-521） | `DoneEvent` 由循环终态判定发射，语义上晚一整段 |
| 未知 chunk 类型 | 忽略（agent/agent.go:515-517 的 default 分支） | 向前兼容 |

边界划在哪、为什么：**事件层的粒度是"消费者可行动"，chunk 层的粒度是"协议事实"，两层刻意不对齐**。可行动性判据逐条看：文本与推理增量对 UI 是可渲染的（透传）；单个 tool call 碎片不可行动——模型还在吐参数，此时发"工具调用"事件既不完整又有执行歧义，所以攒到整流装配完再宣告批；加密推理项不可解释，发事件只会诱导消费者展示乱码；Done 的"可行动终局"属于循环调度而非协议。第 10 篇 §3.5 论证了这条分界换来的稳定性：事件面密封七种（docs/DESIGN.md:125 冻结），协议差异被吸收在 chunk 层。

装配器 `toolCallAccum`（agent/accumulator.go:27）是这条边界的执行者，它要维持的不变量：重复的 `ToolCallStartChunk` index、缺 id/name 的调用、孤儿 args（有 args 无 start）都是**流损坏**，`assemble` 返回错误而不是尽力而为（agent/accumulator.go:99-136）——半条工具参数拼出来的调用去执行比报错危险。有 reasoning items 时按 `output_index` 跨类排序（indexed merge，agent/accumulator.go:247），否则维持 CC 的分桶顺序（reasoning → text → tool calls，agent/accumulator.go:235）。

### 3.4 宽容与严格的分界（chunk 层 vs 事件层）

chunk 层对未知类型宽容（协议演化），事件层对未知事件严格（消费者契约，`foldRunStream` 对未知事件返回错误，agent/agent.go:366-368）。llm 层内还有第二组同样的对比：`Chunk` 未知宽容 vs `Message.UnmarshalJSON` 未知报错（§2.3）。两组的分界符合同一个原则——**面对不可控演化（服务器行为）宽容，面对自有契约（历史数据、事件集）严格**。

### 3.5 非流式回退：一条累积路径

`ChatStream` 报 `ErrStreamingNotSupported` 时，agent 循环回退到 `Chat`，然后把非流式 `*Message` 用 `synthesizeChunks`（agent/provider_call.go:104）**回放成 chunk 序列**再喂进同一个累积器。这保证下游只有一条处理路径——"流式与非流式的差异"被压缩在 provider 调用 seam 之内，累积器、装配、事件发射全部复用（单一执行路径不变式的数据层投影，第 10 篇 §1）。这也是为什么 `Chat` 的返回签名是 `(*Message, *Usage, error)` 而非更多形态：非流式结果一定能无损折叠为块模型 + usage。

## 4. APIError：叶子错误

### 4.1 形状与"为什么是叶子"

```go
// llm/errors.go:9
type APIError struct {
    StatusCode   int
    Body         string
    RetryAfter   time.Duration
    NonRetryable bool   // 覆盖状态码策略，例如配额耗尽
}

func (e *APIError) Error() string  { /* "api error (status %d): %s" */ }
func (e *APIError) Retryable() bool { return !e.NonRetryable && (e.StatusCode == 429 || (e.StatusCode >= 500 && e.StatusCode < 600)) }
```

**没有 `Unwrap`**。它表达的是"一次远端 HTTP 交互的终局事实"：状态码、原始 body、服务器建议的等待时长。这些事实之下没有更小的可组装错误——传输层的 `net.Error`/`url.Error` 不会进 `APIError`（它们走 `IsNetworkError` 的另一条分类，llm/retry.go:86），业务分类也不该在叶子里分叉。做成叶子还换来两个确定性：

- `errors.As(err, &apiErr)` 一定停在准确层级，不会被多层包装链带偏；
- 字段集合稳定。DESIGN.md 明确记录了一个约束："新增公开字段会影响外部 unkeyed literal"（docs/DESIGN.md:349）——所以加字段（如 NonRetryable 引入时）是有兼容成本的决策，不是随手扩展。

被否决的替代形态：一是把 retryable 判断做成包级函数内嵌在重试器里——那样每个 adapter 和包装器都得自己重新发明"哪些状态码可重试"，glm 的 `finishReasonError`（§4.3）就没法用状态码语义说话；二是子类化出 `RateLimitError`/`QuotaError` 等——分类诉求实际上由两个消费者（重试器、UI 提示）分别持有，一个布尔逃生阀 + 状态码字段比继承树便宜得多。

### 4.2 Retryable 的判定标准：状态码策略 + 一个逃生阀

基准策略是**纯状态码**：429（限流）与 5xx（服务器错误）可重试，其余不可。用状态码而不是解析 body：状态码是所有厂商唯一保证存在且语义一致的分层，body 格式每家都不同。

`NonRetryable` 是策略的逃生阀：某些业务终局会**伪装成**可重试状态码——最典型的是配额耗尽常返回 429，但重试它毫无意义，只会烧退避时间。codex 的错误分类器在识别到 quota 类错误码（`usage_limit_reached`/`insufficient_quota`/`quota_exceeded`）时在 `APIError` 上置 `NonRetryable=true`（llm/codex/http_errors.go:20），然后包进 `codex.Error{Kind: quota}`——状态码仍是真实的 429（日志与 UI 不断言错误的事实），重试器却知道这是死路。DESIGN.md 对此的表述："quota 使用真实 HTTP 状态且 NonRetryable=true，未知 429 仍可重试"（docs/DESIGN.md:349 段内）。

### 4.3 跨包消费链：APIError 如何被 errors.As 消费

`APIError` 的消费方全部通过 `errors.As` 拿到它，从不要求调用方直接持有叶子：

| 消费方 | 位置 | 用途 |
|---|---|---|
| 重试分类 `IsRetryableError` | llm/retry.go:74 | `errors.As` → `Retryable()`，决定进不进重试循环 |
| 退避等待 `WaitForRetry` | llm/retry.go:50 | `errors.As` 读 `RetryAfter > 0` 时优先用服务器指示的等待时长，否则抖动指数退避 |
| UI 脱敏 `SanitizeRetryReason` | llm/retry.go:118 | 按 StatusCode 归类文案，**绝不回显 Body**——敏感细节只进 slog |
| 网络分类 `IsNetworkError` | llm/retry.go:86 | 见到 `*APIError` 显式返回 false：API 错误与网络错误是两条互斥的重试理由 |
| agenttest 录制/回放 | DESIGN.md:417 | DTO 携带 StatusCode/RetryAfter/Body 重建叶子，回放保留 `errors.As` 分类 |

两个设计后果值得点出。**其一，包装器守则**：DESIGN.md 要求 Provider 包装器 `%w` 包装、原样透传迭代内错误，"保持 `errors.Is/As` 对哨兵与 `*llm.APIError` 的识别"（docs/DESIGN.md:62）——叶子语义依赖整条包装链不吞错，这是写进契约的义务。**其二，协议级失败也汇入同一分类**：glm 把非 HTTP 的 `finish_reason` 异常翻译成 `APIError`（llm/glm/adapter.go:402-412：`network_error` → 503 可重试、`model_context_window_exceeded` → 400 不可重试）——服务器"流正常结束但结果是失败"这种 CC 特有形态，被映射到状态码域后自动获得正确的重试语义，无需重试器认识 GLM。

叶子上层的分类由**包装型错误**承担，适配器家族里只出现一例：`codex.Error{Kind, Code, RetryAt, Cause}`（llm/codex/errors.go:18）带 `Unwrap` 保留 cause 链，Kind 区分 auth/quota/capability/protocol；它的 `auth.Error`（Stage/Code/LoginRequired）同理。分工很清晰：**叶子承载事实，包装器承载领域语义**；`llm.APIError` 保持零厂商词汇。

## 5. 四个适配器概览

### 5.1 共同骨架与"复制是有意的"

四个适配器都实现 `Provider`，共享 `llm.DoJSONRequest`/`DoStreamRequest`/`ScanSSEEvents` 传输管线（第 21 篇），都带 120s 默认客户端超时。glm 与 openai 的转换助手（`convertMessages`/`parseStreamPayload`/`firstNonEmpty`/`encodeBase64`/`ptrToString`）**逐字重复是设计决定**（llm/AGENTS.md 反模式条目）：两家的 wire 协议今天同构、明天分岔，抽共享 helper 会把"改一家"变成"改两家还要评估另一家"；正确姿势是改 glm 时同步改 openai，diff 里两侧对照反而可审。

结构裁决同样来自 DESIGN.md（2026-09-11 D1）：**不做双协议混包**——"协议差异是条目级的"（docs/DESIGN.md:252），CC（openai/glm）与 Responses（openairesponses/codex）是两个数据形状，各开子包，共享的只是 `Provider` 契约与传输层。

### 5.2 openai（Chat Completions 基准）

`Name()` = `"openai"`（llm/openai/adapter.go:50 起）。数据形状要点：

- **出站压扁**：单 TextBlock 发纯字符串 content，多块/图片发 content parts 数组（llm/openai/adapter.go:331）；assistant 的多个 TextBlock 以 `\n` 连接压回一个字符串。
- **reasoning_content 双向**：入站把响应的 `reasoning_content` 转成 `ReasoningBlock`，出站再把 `ReasoningBlock` 透传回 `reasoning_content`（llm/openai/adapter.go:409），服务 DeepSeek thinking 这类 OpenAI 兼容端点；`ReasoningItemBlock` 出站跳过。
- **纯推理回复合法**：`convertResponseMessage` 在空检查**之前**追加 ReasoningBlock（"reasoning-only response is valid"）——只有既无 reasoning 又无 content 又无 tool_calls 才报 empty message。
- **惰性流**：请求/建连错误在首次 range 时才 yield（llm/openai/adapter.go:171 注释 "preserving the provider's lazy-streaming contract"），这让包装器无法用 pre-range 探测把错误提前重试化——流内错误不重试的边界（第 10 篇 §5）靠它保持。
- **usage 显式请求**：流式时置 `stream_options.include_usage`（llm/openai/adapter.go:181），终包带 usage。

### 5.3 glm（智谱 BigModel）：auth sniffing 与其陷阱

`Name()` = `"glm"`。协议上是 CC 家族（所以与 openai 复制同构），差异集中在四处：

1. **认证双模与嗅探（最著名的陷阱）**。构造期做 auth sniffing（llm/glm/adapter.go:104-108）：`strings.Contains(apiKey, ".")` → JWT 模式，否则 bearer 模式。原因是 BigModel 平台历史上签发 `id.secret` 形态的 key，必须本地用 secret 段自签 HS256 JWT（header `{alg:HS256, sign_type:SIGN}`，claims `{api_key, exp(ms), timestamp(ms)}`，llm/glm/auth.go:25 `generateToken`）；新形态 key 是纯 bearer。
   **陷阱**：一个恰好含点的 bearer key 会被拆成 `id.secret` 去自签 JWT，随后被服务器拒绝——bearer key 带点必然误认证。且**没有覆盖选项**：`glm.ProviderOption` 列表里没有强制 auth 模式的入口（构造期归一化约定），含点 bearer key 在本适配器不可用。这是把"用户不用再传一个 mode 参数"的便利卖给了一个不可检测的歧义；换来的简单是否值得，留待后续裁决，当前行为如实记录于此。
   JWT 有缓存：`tokenCache`（sync.Map）按 key 缓存，到期前 5 分钟内才重签（llm/glm/auth.go:84 `getToken`）；`timeNow`（llm/glm/auth.go:14）是测试时钟 seam。
2. **出站剥离全部推理块**：GLM 服务器不接受 reasoning 回传，`convertStandardMessage`/`convertAssistant` 对两种推理块都静默 strip（llm/glm/adapter.go:456/:483）——降级契约表的一格。
3. **finish_reason 错误化**：`network_error` 映射 503 APIError（可重试）、`model_context_window_exceeded` 映射 400（不可重试）（llm/glm/adapter.go:402-412）——CC 协议"流正常结束但结果是失败"的 GLM 特有形态，§4.3 已述其重试语义收益。
4. **参数收紧**：`Stop` 截断到 1 个（GLM maxItems:1，llm/glm/adapter.go:165-170，截断记 debug 日志）；thinking 配置有构造期与调用期两级、调用期覆盖构造期（含 `ClearThinking`）；`WithTopP` 就地夹取 [0.01,1.0]（§1.3 所述唯一例外）。

### 5.4 openairesponses（OpenAI Responses 协议）

`Name()` = `"openai_responses"`（llm/openairesponses/adapter.go）。包注释（:1-16）完整声明了 v1 边界：**不支持**内建工具（web search/file search/code_interpreter/computer use/MCP）、结构化输出（text.format）、`previous_response_id`、WebSocket。

数据形状上与 CC 的三个本质差异：

- **typed items 而非 messages**：一条 assistant 消息展开成 item 序列（`assistantItems`，llm/openairesponses/convert.go:129）——`ReasoningItemBlock` → `reasoning` item（加密内容原样、置于其 function_call 之前保声明顺序）、连续 TextBlock 合组成一个 `message` item、每个 ToolUseBlock 独立成 `function_call` item；tool 消息成 `function_call_output`（call_id 配对）。system 消息不进 items，升格为顶层 `instructions`，**最后一条生效**（convert.go:73 注释 "the last one wins, matching single-instruction semantics"）。
- **无状态策略（裁决 D2）**：`store:false` + 每次全量回放 items，并 `include: reasoning.encrypted_content`（llm/openairesponses/convert.go:235-236）——历史始终由调用方持有，与 Compactor/截断/Store 正交；`previous_response_id` 与 Conversations API 被否决为 v1 方案（B 方案留作未来可选模式，docs/DESIGN.md:252）。
- **选项的显式拒绝**：`WithStop` 在 Responses 协议无对应参数，返回类型化错误 `ErrStopUnsupported`（llm/openairesponses/convert.go:221）而非静默丢弃——"静默丢掉调用方的显式约束不可接受"。非流式 `Chat` 对 `status != "completed"` 一律报错，不把部分输出当成功（stream.go Chat）。

流式翻译按 DESIGN.md 冻结的事件映射表（docs/DESIGN.md:261）：`output_text.delta` → TextDeltaChunk（带 output_index）、`reasoning_summary_text.delta` → ReasoningDeltaChunk、`output_item.added`(function_call) → ToolCallStartChunk、`function_call_arguments.delta` → ToolCallArgsChunk、`output_item.done`(reasoning) → ReasoningItemChunk、`response.completed` → 补发未交付的 reasoning 项 + DoneChunk（usage）——补发是安全网，正常路径在条目自身 done 时就交付（llm/openairesponses/stream.go:143 `parseStreamEvent` 的 `delivered` 去重表）。未知事件类型零 chunk（向前兼容）。

### 5.5 codex（ChatGPT 订阅 Responses）

`Name()` = `"codex"`（llm/codex/adapter.go）。包注释（:1-10）声明定位：**只走 ChatGPT 订阅 OAuth**，API-key 计费用 openairesponses，不自动发现凭据、不切换计费。凭据由 `auth.Source` 接口注入（`Token`/`Refresh`，持久化归调用方）——llm/codex/auth 只实现非交互协议，交互登录在 codexauth 卫星（docs/DESIGN.md:266-272）。

与 openairesponses 同协议但四处刻意不同：

- **instructions 拼接而非末条生效**：codex 把全部 system TextBlock 以空行连接（llm/codex/adapter.go:5，包注释明示与 openairesponses 的 last-system 行为对照）——订阅端点期望单一 instructions 域，多条 system 是调用方组织习惯，不该丢。
- **本地严格校验**：`convertMessages`（llm/codex/convert.go:15）对畸形块报 capability error 而非尽力转换——nil 块、缺 ID 的 reasoning item、缺 ID/Name/非法 JSON 参数的 tool_use、角色错位的块，全部显式拒绝。理由：订阅端点的错误恢复能力差，与其把畸形数据发上去收一个含糊的 4xx，不如本地 fail-fast 给出可定位的 code。
- **MaxTokens 接受但从不发送**：请求结构完全不含 max_tokens 字段，输出上限由服务端控制；首次消费正数上限时每 Provider 最多告警一次（`warnLimit sync.Once`）——"配置不是远端硬上限"必须对用户可见（llm/codex/adapter.go:8 与 DESIGN.md 输出限制节）。
- **流的状态机校对**：`streamState`（llm/codex/stream_state.go:31）按 output_index 逐条目登记/完成，`complete`（stream_completed.go:9）对账整个输出数组——缺终态、incomplete、矛盾终态、非法 JSON 参数都是 protocol 错误；output_index 可不连续但原值与相对顺序必须保留。`Chat` 归并同一条校对过的流（llm/codex/chat.go:13），保证流式/非流式同一形状。

错误处理（§4 已述叶子）：`classifyServerError`（llm/codex/http_errors.go:10）把 HTTP 401 归 auth、400/403/404/422 归 capability、quota 码归 quota 并置 NonRetryable；只有 pre-stream 401 走一次 Refresh(&used) 重发，其余不重试——惰性迭代内的错误不会触发 agent 外层 pre-stream 重试（这是与第 10 篇 §5 重试边界的对齐点）。

## 6. 相邻文档

- **第 10 篇（自主循环）**：七种密封事件、TC-first、`DoneEvent.History` 权威快照、重试只覆盖 pre-stream——本文 §3 的映射表在事件侧的完整论证。
- **第 21 篇（传输与重试管线）**：`DoJSONRequest`/`DoStreamRequest`/`ScanSSEEvents`、有界 I/O（10MB body / 1MB SSE 行）、退避计算、`Backoff`——本文刻意只写到接口交界。
- **第 14 篇（运行中控制面）**：`SetThreadConfig` 按 `Name()` 解析 provider、fail-closed `ErrUnknownProvider`——`Name()` 字符串的另一半消费者。
- **第 12 篇（持久化）**：history 的落库与回放、`config_changed` 记录如何携带 provider 切换。
- docs/DESIGN.md：openairesponses 裁决 D1–D5（:252-264）、codex 契约（:266 起）、跨厂商降级冻结表（:236）、APIError 约束（:349）。
