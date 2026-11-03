# 40 · 内核不变量与守护

> 内核文档集第 40 篇。前面各篇讲"内核怎么做一件事"（10 循环、11 工具、12 持久化、21 传输……），本文收拢的是横向纪律：七条贯穿全部内核包的不变量，每条回答三个问题——它是什么、谁在守护、破坏之后会发生什么。守护不只是 `scripts/` 下的两个脚本：一部分不变量由类型系统（密封接口）与错误契约（哨兵 + `%w`）承担，一部分由测试逐字断言承担，机器守不住的部分才落在 AGENTS.md 的反模式清单与 review 惯例里。每节末尾给一张统一格式的表：不变量 → 守护机制 → 破坏后果。
> 行号锚点基于 2026-10-29 工作区（基线 65fea54，含 provider 热换未提交改动），全部经 grep 逐条校验；接口契约以 docs/DESIGN.md 为准，本文与其冲突时以 DESIGN.md 为准。

## 1. 零依赖闭包：内核四包 stdlib-only

### 1.1 不变量

`agent` / `llm` / `store` / `tool` 四个内核包的**完整编译闭包**（含传递依赖）只允许使用 Go 标准库；MCP SDK（`github.com/modelcontextprotocol/go-sdk`）的 source import 只允许出现在 `mcp/` 包（package 名 `mcpbridge`）；根 `go.mod` 必须**直接** require 该 SDK 的固定版本 `v1.7.0`。这三句话同时写在契约（docs/DESIGN.md:25，设计目标 3「小」）与守护脚本里。

卫星层不受此限：`harness/` 可以用 Bubble Tea，`mcp/` 可以用 SDK——依赖只允许从卫星流向内核，方向不可逆（第 00 篇 §2 的依赖方向图）。`go.mod` 顶层的 bubbletea 系依赖全部服务于 `harness/`，与内核闭包无关。

### 1.2 守护脚本逐条断言什么

`scripts/kernel-closure-guard.sh` 只有三个 `test`，每个对应断言的一条：

| # | 断言 | 脚本做什么 | 挡住什么 |
|---|---|---|---|
| A | 内核闭包为空 | `go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}}{{end}}{{end}}' ./agent ./llm ./store ./tool`，排序去重后必须为空（kernel-closure-guard.sh:4-7） | 四个内核包的编译闭包里出现**任何**第三方 module——包括测试引入、传递依赖、间接拉进 |
| B | SDK 版本与地位逐字节钉死 | `go list -m -f '... {{if .Indirect}}indirect{{else}}direct{{end}}' github.com/modelcontextprotocol/go-sdk` 的输出必须逐字节等于 `github.com/modelcontextprotocol/go-sdk v1.7.0 direct`（kernel-closure-guard.sh:9-12） | 版本漂移（顺手 `go get -u`）与 direct 变 indirect（SDK 从 go.mod 顶层被挪走——那意味着有别的库替它做了 require） |
| C | SDK importer 白名单 | `go list -test` 扫全仓库（普通 Imports、TestImports、XTestImports 全算；ForTest 重写后的真实包名参与判定），import 了 SDK 的包解析后必须是 `github.com/dailz1/go-agent/mcp` 或其子包（kernel-closure-guard.sh:14-30） | 任何其他包——**包括测试文件**——绕过 `mcp/` 直接 import SDK |

三条缺一不可：A 守内核本身，C 守「SDK 只从 mcp/ 进」，B 守「mcp/ 用的 SDK 版本是被裁决过的」。C 特意带上 `-test` 与 ForTest 解析，是因为测试文件里一条 `import mcp "github.com/modelcontextprotocol/go-sdk"` 在普通 `go list` 下不可见，却是依赖面的真实组成部分。

`mcp/` 自己也把这条写进了包级契约：mcp/AGENTS.md 开篇即「The ONLY package allowed to import the MCP SDK (guard-enforced)」——文档与脚本互为引用。

### 1.3 为什么用 CI 守护，而不是写进约定就够

按分量排四个理由：

1. **闭包在 code review 里不可见。** 一行 `import "github.com/xxx/yyy"` 足以把整棵依赖树拖进闭包，而 diff 里它和任何一个普通 import 长得一模一样；传递依赖更是任何人都看不见。`go list -deps` 是唯一可靠的观察者。
2. **约定的腐化只是时间问题。** AGENTS.md 的反模式清单防的是「不知道」，CI 防的是「忘了」和「没看」。仓库自己的注记就是证据：`go list -m all` 约 40 行（2026-10-29 本机实测恰为 40），绝大多数是 harness 的 bubbletea 系卫星依赖；root AGENTS.md 特意注明旧的「14 行」说法已过时、guard is the authority——数字随卫星增长会继续变，机器断言不变。
3. **逐字节钉死让升级必须走显式通道。** 断言 B 是字节级比较，SDK 从 v1.7.0 到 v1.7.1 的升级会让 CI 红。这是流程设计而非保守：SDK 升级意味着 `mcp/` 的 fidelity 边界（SDK decode surface，第 00 篇 §4）可能移动，必须连同断言、测试、DESIGN.md 一起改，而不是一次例行 `go get -u` 的副产品。
4. **这是供应链性质，不是风格偏好。** 内核的使用者是「在自己的进程里」嵌入 go-agent 的人（docs/DESIGN.md:6）；闭包每多一个 module，他们的依赖审计、漏洞扫描、升级协调就多一份。40 个 module 的审计面与「零」之间没有可以含糊的中间态。

### 1.4 小结表

| 不变量 | 守护机制 | 破坏后果 |
|---|---|---|
| 内核四包编译闭包 stdlib-only | kernel-closure-guard.sh 断言 A：`go list -deps` 结果必须为空 | 嵌入式使用者的依赖审计面从零膨胀；「拷进任何模块零成本」的产品定位失效；`go get` 冲突面扩大到使用者无法控制的传递依赖 |
| SDK source import 仅限 `mcp/` | 断言 C：`go list -test` 全仓 importer 白名单（含测试 import） | SDK 类型面从多个包渗入公共 API；裁剪 MCP 支持或升级 SDK 时破坏面不可控 |
| `go.mod` 直接 require SDK `v1.7.0` | 断言 B：输出逐字节等于 `... v1.7.0 direct` | 版本静默漂移，`mcp/` 行为随上游变化而无人知道该复核什么 |

## 2. 命名后缀分类法：后缀即生命周期

### 2.1 不变量

内核公共类型的名字带一个受控后缀，后缀标明这个值**活在哪一层、生命周期是什么**：

| 后缀 | 含义 | 类型清单（锚点） |
|---|---|---|
| `*Block` | `llm.Message` 里的**内容块**——已定型的完整内容 | `ContentBlock`（llm/message.go:29）及 `TextBlock` :34、`ReasoningBlock` :42、`ImageBlock` :50、`ToolUseBlock` :60、`ReasoningItemBlock` :74、`ToolResultBlock` :84 |
| `*Chunk` | Provider 流上的**增量**——按 stream Index 重组的碎片 | `Chunk`（llm/chunk.go:20）及 `TextDeltaChunk` :26、`ReasoningDeltaChunk` :41、`ToolCallStartChunk` :51、`ToolCallArgsChunk` :66、`ReasoningItemChunk` :82、`DoneChunk` :93 |
| `*Event` | agent 层交给消费者的**事件**——观测与回放的唯一入口 | `AgentEvent`（agent/event.go:15）及七个密封变体 `TextDeltaEvent` :21、`ThinkingDeltaEvent` :30、`ToolCallEvent` :46、`ToolResultEvent` :60、`RetryEvent` :89、`DoneEvent` :98、`CompactionEvent` :126 |
| `*Result` | 工具执行的**结果**——软错误载体 | `ToolResult`（tool/tool.go:93），构造器 `NewErrorResult` :101、`NewTextResult` :109、`NewDataResult` :116 |
| `*Config` | 配置聚合体——纯数据，无行为 | 例：`agent.ConfigChange`（agent/config.go:34）、`agenttool.Config`（agenttool/agenttool.go:26）、`mcpbridge.Config`（README.md:132 的用法示例） |

与后缀平行的是**构造动词**：`Message` 层按角色命名——`SystemMessage` / `UserMessage` / `AssistantMessage` / `AssistantToolCallMessage` / `ToolResultMessage`（llm/message.go:93-126）；`Result` 层按载荷命名——`NewTextResult` / `NewDataResult` / `NewErrorResult`（tool/tool.go:101-116）；`Block` 无 `New*` 构造器，字面量加密封方法 `blockType()`（llm/message.go:39-91）即是全部构造面。

分类法真正的力量在**前缀与后缀的正交**：同一个概念在三层各有一个名字，前缀对齐概念、后缀对齐层次。文本是 `TextBlock`（内容）→ `TextDeltaChunk`（流增量）→ `TextDeltaEvent`（agent 事件）；工具调用是 `ToolUseBlock` → `ToolCallStartChunk` + `ToolCallArgsChunk` → `ToolCallEvent`；工具结果是 `ToolResultBlock`（消息内）→ `tool.ToolResult`（执行产物）→ `ToolResultEvent`（事件）。看到名字就知道：这个值该被直接放进历史、还是按 Index 重组（accumulator，agent/accumulator.go）、还是按 TC-first 语义交付给消费者（第 10 篇 §3）。

### 2.2 为什么严格分类

消费代码对三层的处理方式完全不同：`Block` 直接进 `[]llm.Message`；`Chunk` 必须经 `toolCallAccum` 按 stream Index 重组（agent/accumulator.go）；`Event` 按「一轮的 ToolCallEvent 全部先于任何执行」（TC-first，agent/AGENTS.md「CONVENTIONS」）交付，且 `DoneEvent.History`（agent/event.go:103）才是权威快照——增量事件不足以重建含 reasoning 的 assistant（第 12 篇 fold 契约）。后缀就是这三种命运的标签：名字错一个后缀，读者要么打开定义要么猜错，而且猜错的方向恰好是「把增量当完整内容」这类最难查的 bug。

这条不变量没有专门的 guard 脚本，守护分两层：一是 grep 可机械核对（`^type \w*Block` 只在 llm/message.go 命中、`*Chunk` 只在 llm/chunk.go 与 adapter、`*Event` 只在 agent/event.go），review 依据它做例行检查；二是密封接口兜底——每个联合的 marker 方法（`blockType()` / `eventType()`）未导出，包外根本无法实现，新增类型只能进拥有该分类法的包，放错包即编译失败。

### 2.3 小结表

| 不变量 | 守护机制 | 破坏后果 |
|---|---|---|
| 后缀标层次：`*Block` 内容 / `*Chunk` 流增量 / `*Event` agent 事件 / `*Result` 工具结果 / `*Config` 配置 | grep 惯例 + 密封 marker 方法（`blockType()` llm/message.go:39、`eventType()` agent/event.go:26-134）限制类型只能定义在拥有分类法的包 | 名字失去信息量：`TextDeltaEvent` 与 `TextDeltaChunk` 若可混用，消费者会把流增量错当 agent 事件处理，重组与 fold 契约（第 12 篇）静默错位 |
| 前缀对齐概念：同一概念跨层同名前缀 | review 惯例（无脚本） | 跨包对读成本上升：`ToolUseBlock` / `ToolCallEvent` / `ToolResultBlock` 的对应关系要逐个打开文件才能确认；文档集（10/11/20/21 篇）的引用体系失效 |
| 构造动词平行：Message 按角色、Result 按载荷 | review 惯例 | 调用方记忆负担：每个类型一套临时构造方式，`ToolResultMessage(llm/message.go:126)` 的对称性消失 |

## 3. 错误处理：`%w` 链、哨兵、叶子错误、零副作用

### 3.1 不变量与代码形态

四条规则，每条都有可 grep 的形态：

1. **包装错误用 `fmt.Errorf("lowercase context: %w", err)`**：小写开头、给上下文、`%w` 保链。内核里随处可见——`llm/openai/adapter.go:103` 的 `convert messages: %w`、`:110` 的 `convert tool definitions: %w`；agent/config.go:72 的 `agent: provider name must not be empty: %w`；agent/compact.go:353/:471/:544 把预算超限包成 `sliding window: %w`、`summarization: %w`。
2. **可判定的失败用哨兵 + `errors.Is`**：`llm.ErrStreamingNotSupported`（llm/provider.go:14，供调用方走非流回退）、`agent.ErrUnknownProvider` / `agent.ErrInvalidConfigChange`（agent/config.go:26/:19）、`agent.ErrInvalidHistory` / `agent.ErrCompactionBudgetExceeded`（agent/compact.go:16/:18）、`agent.ErrNothingToResume`（agent/persist_id.go:28）。哨兵永远配 `%w` 包装使用，`errors.Is` 穿透到底。
3. **HTTP 失败用叶子错误 `*llm.APIError` + `errors.As`**：`APIError`（llm/errors.go:9）携带 `StatusCode` / `Body` / `RetryAfter` / `NonRetryable`，`Retryable()`（llm/errors.go:23）——429 或 5xx 且未被显式标死——是重试分类的**唯一决策点**。它是叶子：llm/errors.go 全文没有 `Unwrap`（grep 验证），`errors.As(err, &api)` 命中的就是最终 HTTP 事实，不存在再往下解的层。
4. **库代码禁 `init()`、禁读环境变量**：`^func init` 在四个内核包零命中（agent/llm/store/tool 全量 grep）；`os.Getenv` 只出现在 `//go:build e2e` 的测试（agent/agent_e2e_test.go:1 标签、:193 读 `OPENAI_API_KEY`；llm/openairesponses/e2e_test.go:1）与 `examples/`（examples/agentcli/main.go:48-54）。配置一律走 functional options 显式注入，在 `agent.New`（构造 `Option`）与 `newRunConfig`（逐调用 `RunOption`）归一。

### 3.2 为什么是这三层分工

`%w` 保链是为了调用方的判定权：agent/agent.go:432 把 `ErrUnknownProvider` 包上线程与 provider 名字再抛出，消费者既要读得到人话上下文，也要 `errors.Is(err, agent.ErrUnknownProvider)` 依然成立——丢了链，热切换失败的分支处理就只能做字符串匹配。

`APIError` 刻意做叶子，因为它已经是分类终点：`Retryable()` 决定 `chatWithRetryAndFallback`（agent/provider_call.go:13，唯一 provider 触点）是否在 pre-stream 阶段重试。若它带 `Unwrap` 链，`errors.As` 可能先命中外层类型，重试分类就出现了第二个决策点——恰好是「只 pre-stream 重试、mid-stream 不重试」（第 21 篇）这条规则要消灭的东西。

禁 `init()` 与 env 是同一件事的两面：库在 import 时不得有副作用，行为不得依赖进程环境。`init()` 里的注册让依赖顺序变成隐式全局状态；库代码读 env 则让同一段代码在不同机器上行为不同，且测试无法隔离——这正是「可替换」目标（docs/DESIGN.md:24）的反面。env 只允许出现在两类进程边界：e2e 测试（显式 build tag 隔离，默认 `go test ./...` 不触网不读环境）与 examples（examples 本身是 `package main`，是入口程序不是库）。

### 3.3 小结表

| 不变量 | 守护机制 | 破坏后果 |
|---|---|---|
| 包装错误 `%w` 保链、小写上下文 | review 惯例；下游与 e2e 对 `errors.Is` 的断言 | 链一断，哨兵判定退化为字符串匹配；热切换/预算/持久化分支处理全部脆化 |
| 哨兵错误 + `errors.Is` 判定分支 | 哨兵定义处注释明示用法（llm/provider.go:12-14）；非流回退的真实消费点在 agent/provider_call.go:39（`errors.Is(lastRetryErr, llm.ErrStreamingNotSupported)`） | 调用方无法区分「可回退」（不支持流式）与「该放弃」（配置非法），错误处理面膨胀成 switch 字符串 |
| `*llm.APIError` 叶子无 `Unwrap`，`Retryable()` 唯一决策点 | llm/errors.go 无 Unwrap（grep 验证）；重试测试经 `errors.As` 断言（llm/codex/adapter_test.go:140） | 重试分类出现第二个入口：mid-stream 错误被误判可重试，或 429 被误判放弃，预算与稳定性双输 |
| 库代码禁 `init()`、禁 env 读取 | grep 可验证（四包零命中）；e2e 用 `//go:build e2e` 隔离（agent/agent_e2e_test.go:1） | import 产生副作用、行为依赖进程环境；测试在不同机器结果不同；「可嵌入」变成「只能在我这台机器嵌入」 |

## 4. 流式契约：公共 API 无 channel，一律 `iter.Seq2`

### 4.1 不变量

所有流式公共 API 只有一种形态：返回 `(iter.Seq2[T, error], error)`——首个 error 是**建立流之前**的失败（同步返回），迭代中的错误走 Seq2 的第二列。`llm.Provider.ChatStream`（llm/provider.go:30）返回 `iter.Seq2[Chunk, error]`；`Agent.RunStream` / `RunStreamWithHistory`（agent/agent.go:384/:879）返回 `iter.Seq2[AgentEvent, error]`；`Run`（agent/agent.go:335）不是例外——它是事件流的归并（`foldRunStream`，agent/agent.go:343），「流是执行原语，同步执行是衍生物」（docs/DESIGN.md §4）。四个 adapter 全部照此：llm/openai/adapter.go:172、llm/glm/adapter.go:251、llm/codex/adapter.go:90，内核内部管道如 `scanResponse`（llm/codex/stream.go:13）同形。`llm/provider.go` 里 grep `chan ` 零命中；agent/AGENTS.md 反模式清单明令「Do not add channels to public APIs」。

### 4.2 为什么禁 channel

`iter.Seq2` 由 `range` 驱动，三个性质是 channel 给不了的：

- **惰性**：不 range 就不执行。SSE 连接在第一次迭代时才建立，包装器守则因此可以要求「流式包装器不得预先 range，保持 `iter.Seq2` 惰性与早退安全」（docs/DESIGN.md §4 包装器守则）——重试窗口与连接生命周期由此可推理。
- **早退安全**：消费者 `break` 即停止，天然传播取消；无需「谁负责 drain 剩余」的约定。channel 版本里 break 之后生产者还在发送，要么阻塞泄漏要么丢数据。
- **零 goroutine 面**：没有生产者 goroutine，就没有 close 纪律、没有泄漏审计。channel 流的实现者必须自己回答「早退后 goroutine 谁收、close 谁调、panic 谁接」，而 `iter.Seq2` 让这些问题在语言层面不存在。

### 4.3 小结表

| 不变量 | 守护机制 | 破坏后果 |
|---|---|---|
| 流式公共 API 返回 `iter.Seq2[T, error]`，禁 channel | grep `chan ` 可验证（llm/provider.go 零命中）；agent/AGENTS.md 反模式清单；adapter 全家同形 | 每个 adapter 自己发明生命周期语义：忘 close 即 goroutine 泄漏，忘 drain 即死锁，消费者 break 之后的行为因实现而异 |
| 建立前错误同步返回、迭代中错误走第二列 | 形态即约定（首个 error 位） | 调用方无法区分「请求没发出去」（可重试）与「流中途断了」（不重试，第 21 篇），重试边界失去依据 |
| 包装器不得预先 range（保持惰性） | docs/DESIGN.md §4 包装器守则；review | 「第一个 chunk 到达前不建立连接」失效，包装层把连接提前到包装器构造时，重试与取消窗口错位 |

## 5. 测试纪律：确定性优先于覆盖数字

### 5.1 不变量

- **stdlib `testing` only**：全仓 grep `testify|stretchr` 零命中；断言手写，表驱动（table-driven）是默认形态。不引断言库也是闭包不变量（§1）在测试侧的延伸——内核测试不得把依赖面扩大到被测代码之外。
- **白盒 in-package**：agent 包测试直接构造未导出的 `mockTool`（agent/agent_test.go:182/:219/:251）、直接触达未导出函数。`agent/mock_provider.go` 的导出构造器建在**未导出类型**上——包外根本用不了，这是有意的边界（agent/AGENTS.md「Provider mocks」行）：外部包请用 `agenttest`。
- **测试 seam 是包级变量**：`llm/retry.go:21` 的 `var sleepFor = func(ctx, delay) error`、`llm/glm/auth.go:14` 的 `var timeNow = time.Now`。重试测试改写 `sleepFor` 即零真实等待；JWT 过期测试注入 `timeNow` 即无需等时间流逝。seam 只在本包内可达——agent/agent_test.go:1580 的注释直言 `llm.sleepFor` 从 agent 包不可达，跨包确定性不靠 seam，靠 `agenttest` 的确定性替身。
- **禁时序运气**：不允许靠 fixed sleep/poll「等它变绿」；CI 用 `go test -race -shuffle=on -count=1 ./agent ./store`（root AGENTS.md COMMANDS）把并发与执行顺序抖动显式暴露。异步行为先订阅确切状态变化再触发动作，有界超时。
- **e2e 隔离**：真实 API 测试全部 `//go:build e2e`（agent/agent_e2e_test.go:1），默认 `go test ./...` 无网络、无环境依赖。

### 5.2 `agenttest/` 的定位：公共替身 vs 包内 mock

两套替身服务两个世界：包内 mock（未导出）让内核测试能直触内部状态；`agenttest/`（卫星层）给下游消费者提供确定性公共替身——`ScriptedProvider`（agenttest/scripted_provider.go:77，`NewScriptedProvider` :85，`Verify` :190，请求严格匹配）、`Replayer`（agenttest/replay.go:17，录制/回放严格含流内位置，agenttest/record.go:89）、`ToolFunc`/`ToolCall`（agenttest/tool.go，带调用记录）。agenttest/AGENTS.md:4 写明分工：内核自己的包内测试不用这个包。典型工作流：录一次真实交换、把纯字节录制提交为 fixture，CI 里零网络回放（examples/testdouble 是样板接线）。

为什么要两套：把包内未导出 mock 导出，等于把内部结构变成公共 API；把 agenttest 塞进包内使用，则测试开始依赖卫星层，闭包不变量（§1）被测试倒着打破。两条路都通向同一个糟糕结果——内核内部形状被外部锁定。

### 5.3 小结表

| 不变量 | 守护机制 | 破坏后果 |
|---|---|---|
| stdlib `testing` only、表驱动 | grep `testify` 零命中；闭包守护（§1）的压力延伸 | 测试自身引入依赖面；断言语义随第三方库版本漂移，flaky 归因变难 |
| 白盒 in-package，包内 mock 不导出 | 导出构造器建在未导出类型上（agent/mock_provider.go）——物理上不可外用 | 内部结构被下游锁定，重构即破坏兼容；每次内部调整都变成公共 API 变更 |
| 时间/等待 seam 是包级变量（`sleepFor`/`timeNow`） | seam 定义处即使用处（llm/retry.go:21、llm/glm/auth.go:14）；agent_test.go:1580 注明跨包不可达 | 重试/过期测试要么真等待（慢），要么靠 sleep 赌时序（flaky） |
| 禁时序运气，`-race -shuffle=on` | CI 命令（root AGENTS.md COMMANDS）；失败必须可重现才可修 | 红灯无法复现，要么被重跑掩过（真回归进主干），要么被永久 quarantine（测试面失去信用） |
| e2e 用 `//go:build e2e` 隔离 | build tag（agent/agent_e2e_test.go:1） | 默认测试触网/读环境：离线 CI 全红或被跳过，e2e 与单测边界消失 |
| `agenttest/` 是面向下游的公共替身 | agenttest/AGENTS.md:4 分工声明；内核包内测试零引用它 | 下游只能自造 mock，对内核行为的理解碎片化；或包内 mock 被迫导出（见上行后果） |

## 6. 文档契约：DESIGN.md 先行、双语 README、`pkg/` 禁令

### 6.1 三个契约

**接口先行，改接口先改 docs/DESIGN.md。** DESIGN.md 开篇即自我定位：「本文档是项目的设计契约：定位、目标、原则、路线图。改接口先改这里」（docs/DESIGN.md:4）。§1 的零依赖闭包本身就是文档条款（docs/DESIGN.md:25）——脚本断言的是契约条文，而不是反过来。内核文档集（本目录 00-40）是对 DESIGN.md 的展开，冲突时以 DESIGN.md 为准（每篇篇首声明这一点）。

**双语 README 契约。** `scripts/check-readme-contract.sh` 对 `README.md` 与 `README.zh-CN.md` 各执行同一套检查：module path 与 `Go 1.26.1`（从 go.mod 读出）必须出现；`agenttool.New` 与 `mcpbridge.Connect` 必须出现（README.md:106/:111/:132 满足）；`git clone` / `cd go-agent` / `export OPENAI_API_KEY='...'` / `go get module@latest` / `go run ./examples/quickstart` 这些上手命令必须**逐字节**存在（`grep -Fqx`，整行匹配）；README 里每个相对链接的目标必须存在（防止文档指向已删除的文件）。两份 README 语义镜像，脚本保证它们都不会悄悄失效。

**`pkg/` 目录禁令。** 本仓库 2026-09-20 从 holmes-go 的 `pkg/` 子树独立提取成库，同日 commit `1b1932b`（refactor: promote pkg/* packages to repository root）把 `pkg/` 提升到模块根。root AGENTS.md ANTI-PATTERNS 明令勿再引入 `pkg/` 目录。这不是美学：import path 已经发布变更过一次（`pkg/agent` → `agent`），回退等于让所有下游再断一次；且 kernel-closure-guard 的路径列表（`./agent ./llm ./store ./tool`）建立在根级布局之上。

### 6.2 小结表

| 不变量 | 守护机制 | 破坏后果 |
|---|---|---|
| 接口变更先改 docs/DESIGN.md | 流程惯例（docs-first，root AGENTS.md CONVENTIONS）；各专篇篇首声明冲突以 DESIGN.md 为准 | 代码与契约漂移：review 无从对照「这是变更还是 bug」；下一个读 DESIGN.md 做设计的人基于幻觉事实工作 |
| 双语 README 逐字节满足上手契约 | `check-readme-contract.sh` 对两份 README 同套断言（含链接目标存在性） | 上手命令拼写漂移（版本号/路径/环境变量），新用户第一步就失败；两份 README 语义分叉 |
| 不再引入 `pkg/` 目录 | root AGENTS.md ANTI-PATTERNS；历史记忆（commit 1b1932b） | import path 二次断裂，所有下游被迫再迁移一次；闭包守护路径列表失效 |

## 7. 审批 fail-closed：无回调即拒绝执行

### 7.1 不变量

一个 `RequiresApproval: true` 的工具，在 Agent 没有配置审批回调（`approvalFn == nil`）时，**拒绝执行**：不调用工具，返回一条软错误 `ToolResult` 回喂给模型。执行点在 `agent.executeTool`（agent/agent.go:934）：

```go
if t.Info().RequiresApproval {                                   // agent/agent.go:940
    if a.approvalFn == nil {                                     // :941
        return tool.NewErrorResult("tool %q requires approval but no approval callback is configured", call.Name), nil  // :942
    }
```

三个设计决定同时压在这几行里：**拒绝而非放行**（fail-closed）；**软错误而非 Go error**——run 不死，模型看到拒绝理由可以改道（第 11 篇软硬错误分工）；**错误文案稳定**——并发执行路径走同一句话（agent/tool_execution_planner.go:52），测试逐字断言（agent/agent_test.go:210 `want := \`tool "sensitive" requires approval but no approval callback is configured\``；配了回调的对照用例 :667）。

### 7.2 跨包一致性

这条语义必须穿过每个「工具从哪来」的边界，三处各自钉住：

- **`tool` 侧**：`RequiresApproval` 是 `ToolInfo` 的字段（tool/tool.go:37），Registry 冻结注册时的 Info 并只发副本（tool/registry.go:246 复制该字段）——审批标记在注册时刻定型，后续篡改不了。
- **`agenttool` 侧**：`agenttool.Config.RequiresApproval` 把外层审批委托给父 Agent（agenttool/agenttool.go:26 的 Config，:31-32 注释明写 delegates，:65 透传进 ToolInfo）。agenttool_test.go:70 用 agenttest Exchange 断言：父 Agent 无审批回调时，子工具收到的是同一句「requires approval but no approval callback is configured」软错误并进入模型可见历史——fail-closed 语义穿过 parent→child 边界不变形。
- **`mcp` 侧**：桥接进来的远程工具**默认全部需要审批**；`WithApprovalRequired(false)` 也只放开 annotations 明确 read-only 且非 destructive 的工具，其余仍然门禁（mcp/README.md:7）。远程工具默认不可信，方向与内核一致：宁可拒绝，不可放行。

### 7.3 为什么必须 fail-closed

「忘记配回调」恰是这条不变量要防的错误形态：如果无回调时静默放行，那么「需要审批」这个安全性质就完全建立在调用方记得配置之上——而忘记配置的那一刻，工具已经带着审批标记执行了，事后无从发现（日志里没有拒绝记录，因为根本没有拒绝发生）。fail-closed 把「配错了」变成一个可观察、可恢复的事件：模型收到拒绝理由，调用方在下轮补上回调即可继续。这直接落在设计原则「默认安全」（docs/DESIGN.md §4）上。

### 7.4 小结表

| 不变量 | 守护机制 | 破坏后果 |
|---|---|---|
| `RequiresApproval` 且无回调 → 拒绝执行，返回软错误 | agent/agent.go:940-942；agent/tool_execution_planner.go:52（并发路径同文案）；agent_test.go:210 逐字断言；agenttool_test.go:70 经 agenttest 断言穿透子 Agent 边界 | 「忘记配回调」变成静默放行：带审批标记的工具已执行，事后无从发现、无从审计——审批机制名存实亡 |
| 拒绝是软错误回喂，不是 Go error | 文案进 `ToolResult`（NewErrorResult，tool/tool.go:101）；第 11 篇软硬错误分工 | run 被掐断，用户看到的是崩溃而非「模型被拒绝了、正在改道」；审批拒绝从可恢复事件变成终态失败 |
| 审批标记在注册时冻结，只发副本 | tool/registry.go:246 | 运行中改写 `RequiresApproval` 可把已注册危险工具变成免审，审批门禁可被工具自身代码绕过 |
| `mcp` 桥接工具默认必审 | mcp/README.md:7（`WithApprovalRequired(false)` 只放开明确 read-only 非破坏） | 远程 MCP 工具的 annotations 不可信时，破坏性调用被直接放行 |

## 8. 现状注记：并非所有缺口都有机器守护

不变量文档的价值不在于宣称一切已守好，而在于把已知缺口记在明处。第 30 篇曾记录一个真实缺口（30-store.md §1.3）：**写侧已接受 SchemaV3**，**但 JSONL 读侧 `load` 仍以 `r.Schema > SchemaV2` 判不支持**——SchemaV3 checkpoint 可落盘，目录 `Close` 后重开即 `ErrUnsupportedSchema`。该缺口已于 2026-10-29 修复（commit 90759e6）：读侧上限对齐 SchemaV3，契约注释同步修订，schema_v3_test.go 以「V3 重开可读回 + 伪造 V4 读侧拒绝」两个测试钉死。它是「版本化信封」不变量（第 12/30 篇）读侧滞后的一个已关闭案例，也印证本文元规则：写读两侧共享常量、以测试回环钉死，比依赖人工同步更可靠。

另有一条元规则贯穿本文：**守护机制的强度应当匹配不变量的破坏隐蔽度**。闭包（不可见）用 CI 脚本；审批（生死攸关、跨包边界多）用逐字断言钉死；命名与构造动词（review 可见）靠惯例 + 密封接口兜底；文档契约靠脚本断言可机检部分（命令逐字、链接存在）。为新代码选守护方式时，先问「破坏它的人能不能在 review 里看出来」。

## 同目录索引

| 篇号 | 主题 | 与本文的关系 |
|---|---|---|
| 00 | 内核全景（总览） | 四包切分与依赖方向，§1 的卫星/内核边界出处 |
| 10 | agent 循环 | TC-first 事件交付、fold 契约的消费侧（§2/§4） |
| 11 | agent 工具 | 软硬错误分工、审批回调（§7） |
| 12 | agent 持久化协议 | 记录语义、恢复语义；SchemaV3 读侧滞后的协议侧（§8，已修复） |
| 13 | agent 上下文 | 截断/压缩预算哨兵的使用场景（§3） |
| 14 | agent 控制面 | 配置热切换哨兵 `ErrUnknownProvider` 的产生场景（§3） |
| 20 | llm 消息模型 | Block/Chunk 分类法的完整展开（§2） |
| 21 | llm 传输 | pre-stream/mid-stream 重试边界、SSE 管道（§3/§4） |
| 30 | store 字节层 | 版本化信封、SchemaV3 读侧缺口修复记录（§8） |
| 31 | 工具 schema 专篇（31-tool.md） | ParameterSchema codec 的 stdlib-only 细则（§1 引用 DESIGN.md:25 的 validator 禁令） |
| 40 | 本文：不变量与守护 | — |
