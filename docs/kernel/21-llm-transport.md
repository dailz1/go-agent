# 21 · HTTP 与 SSE 传输层（llm 包）

> 内核文档集第 21 篇。本文讲 llm 包的传输层：`DoJSONRequest` / `DoStreamRequest` / `ScanSSEEvents` 这条共享 HTTP+SSE 管道、三道 I/O 限额（10 MB 响应体 / 1 MiB SSE 行 / 120 秒客户端超时）的威胁模型与取值理由、流中断错误的浮出路径、pre-stream 重试与降级链的传输层细节、适配器转换代码的复制纪律，以及三种鉴权模式的对比。数据模型（`Message` / `ContentBlock` / `Chunk` 密封联合与 `ReasoningItem` 语义）归第 20 篇；重试的**决策**（退避配置、RetryEvent 记账、fallback 语义论证）归 10-agent-loop.md §5——本文只讲传输层如何感知错误、以什么形状把错误交出去。
> 行号锚点基于 2026-10-29 工作区（基线 65fea54，含 provider 热换改动），与 10-agent-loop.md 同批；接口契约以 docs/DESIGN.md 为准，两者冲突时以 DESIGN.md 为准。

## 1. 传输层边界与限额：10 MB / 1 MiB / 120s

### 1.1 三道数字住在哪里

| 限额 | 值 | 定义点 | 生效点 |
|---|---|---|---|
| 响应体上限 | 10 MB（`MaxResponseBody = 10 << 20`，llm/http.go:20） | llm 包根 | `DoJSONRequest` 读体（:53）、`DoStreamRequest` 读非 2xx 错误体（:116）、codex 的 SSE 单事件累计与原始流量（llm/codex/stream.go:25/:71/:92） |
| SSE 行上限 | 1 MiB（初始缓冲 64 KiB，llm/sse.go:29） | llm 包根 | `ScanSSEEvents` 的 `bufio.Scanner` |
| HTTP 客户端超时 | 120 秒 | 各适配器 `NewProvider` 的默认 client | openai（llm/openai/adapter.go:56）、glm（llm/glm/adapter.go:116）、codex（llm/codex/adapter.go:72）、openairesponses（llm/openairesponses/adapter.go:61） |

三层各拦一种资源：10 MB 拦**内存**（单次读入的字节量），1 MiB 拦**缓冲**（单行增长的上限），120s 拦**时间**（连接占用的时长）。三者没有互相替代的关系——慢速滴注（每秒发一个字节）不违反任何字节限额，只有超时能拦；流式路径根本没有"整体响应体"可拦，只有行缓冲上限能封住单个事件的膨胀。

### 1.2 威胁模型：异常服务器能对进程做什么

传输层不假设服务器善意。一个恶意或故障的 endpoint 至少能做五件事，每件事都有对应的拦截：

1. **返回超大 JSON 响应体**（非流式 `Chat` 路径）。无界 `io.ReadAll` 会把它整个读进内存，一次响应即可 OOM。拦截：`io.ReadAll(io.LimitReader(resp.Body, MaxResponseBody))`（llm/http.go:53），常量注释写明目的就是 "prevent OOM when a provider returns an unexpectedly large payload"（:17-18）。超出 10 MB 的部分被静默截断——这只发生在**非 2xx 错误体**或解码前，截断损失的是错误信息的完整性，不是决策正确性（非 2xx 已注定失败，见 §3.2）。
2. **发送永不换行的超长行**。`bufio.Scanner` 的默认上限是 64 KiB，超长行本会直接报 `bufio.ErrTooLong`；这里显式放宽到 1 MiB（`scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)`，llm/sse.go:29），既容纳偶发的超长 delta，又把"无界缓冲"封死在一个常数上。超限行的结局：`scanner.Scan()` 返回 false，`scanner.Err()` 为 `bufio.ErrTooLong`，作为错误从迭代器浮出（:85-89），流终止。
3. **用多行 `data:` 无限累计一个事件**。SSE 规范允许一个事件由任意多行 data 组成，共享扫描器只限单行、不限累计。codex 适配器为此在扫描器**之下**包了一层 `eventLimitReader`：按字节流跟踪行边界与 `data:` 前缀，事件累计超过 10 MB（llm/codex/stream.go:92）或原始流量超过 2×10 MB（:71）即报错。放在 Reader 层而不是解析层，是因为扫描器把多行 join 成单个分配**之前**是最后能拦住的地方（llm/codex/stream.go:36-38 注释）。
4. **打开连接后挂死不发数据**。没有 `http.Client.Timeout` 时，一个 wedged 连接可以永远占住一个 run——openairesponses 的构造注释把这条理由写进了代码（"an unbounded client could block a run forever on a wedged connection"，llm/openairesponses/adapter.go:53-55）。`Client.Timeout` 覆盖从连接、重定向到**读完响应体**的整个周期；对 SSE 流，这意味着整条流的总时长被 120 秒封顶。
5. **流"正常"结束但从不宣告完成**（不发 `[DONE]`、不给 finish_reason、不发 `response.completed`）。这不是网络错误——EOF 是干净的——所以由适配器在 EOF 后做完整性判定：openai 追踪 `yieldedDone`，EOF 而无 `DoneChunk` 即报 "stream ended without completion"（llm/openai/adapter.go:223/:243-244）；openairesponses 同款守卫（"stream ended without a completed response"，llm/openairesponses/stream.go:124-126）；codex 校验终态事件，缺 `response.completed` 是 protocol 错误（llm/codex/stream.go:32-33，docs/DESIGN.md:343-345）；glm 不在适配器层设此哨兵，"流结束而无 Done"由循环侧装配器终止（见 10-agent-loop.md §3.4 折叠契约）。

### 1.3 取值理由

三个数都不是精确推导的产物，而是"远大于合法最大值、远小于危险区"的量级封顶：

- **10 MB**：错误体与非流式 JSON 响应是纯文本，正常在一两 KB 到几十 KB；10 MB 已容纳任何观察过的合法载荷（含极长的错误描述与 usage 明细），同时离 OOM 区还有三个数量级。同一个常量在 codex 里兼任单事件与完成响应的上限（docs/DESIGN.md:343），一个数字服务两种读法，避免两套限额互相打架。
- **1 MiB 行**：一条 SSE data 行是一个增量 JSON（一个 delta），典型几百字节到几 KB；1 MiB 是它的数百倍余量，又是 `bufio.Scanner` 可以安全一次性分配的上限。
- **120s**：一次完整补全（含长推理）的保守上界。代价是诚实的：超过 120 秒的**单条流**会被 `Client.Timeout` 截断——这是"默认安全"（docs/DESIGN.md:67）与"用户程序不能挂"（:23）之间的取舍。需要更长流的宿主用 `WithHTTPClient` 换上自己的 client（四个适配器都提供该选项），时间边界的所有权随之移交宿主。

### 1.4 未选的替代

- **不设限、依赖调用方传 deadline**：`context.Background()` 是完全合法的调用方式，内核不能把进程安全押在调用方自律上；限额是机制不是策略（docs/DESIGN.md:58）。
- **信任 `Content-Length`**：服务器可以谎报，chunked 编码则没有长度可言；字节计数是唯一不依赖对方诚实的度量。
- **只留 `Client.Timeout` 一道**：它拦时间不拦内存——一个 10 GB 的响应体完全可以在 120 秒内送达并 OOM；反之滴注式流只有超时能拦。二者不可互相替代。
- **流式路径也整读**：等价于把流式变成"等完再给"，摧毁增量交付语义。所以 `DoStreamRequest` 成功路径返回原始 body 供惰性消费，只对非 2xx 错误体整读（llm/http.go:114-116）。

## 2. SSE 解析设计：ScanSSEEvents

### 2.1 一个 SSE 子集状态机

`ScanSSEEvents`（llm/sse.go:26）实现"SSE 规范中与 OpenAI 风格流式 API 相关的子集"（:12-13 注释）：逐行扫描，维护一个 `dataLines` 累计器——

- `:` 开头的行是注释，跳过（:42-44）——服务器用心跳注释保持连接活跃，心跳不是事件；
- `data: ` 与 `data:` 两种前缀都接受（:62-65），取到的载荷行追加进累计器；
- 空行是事件边界：把累计器 join 成一个 payload、清空、dispatch（:46-59）；
- `[DONE]` 哨兵终止流且不产出（:51-53）；
- 其余字段行（`id:`、`event:`、`retry:`）被忽略——本内核的消费方全部按 JSON payload 解码，SSE 的这些字段没有语义落点，实现了也不增加任何行为。

选择"子集"而不是完整 SSE 规范，理由同上：多余字段在内核里没有消费者。解析器的每条行为都有测试钉着（llm/sse_test.go：单事件 :15、多事件 :29、DONE :46、注释 :57、空输入 :67、ctx 取消 :77、多行 data :100、无空行的尾帧 :127/:143/:156）。

### 2.2 部分帧处理：EOF 冲刷

连接在事件中途断开时，累计器里可能留着没等到空行的半帧。`ScanSSEEvents` 在 `scanner.Scan()` 循环自然退出后冲刷累计器（:71-81）：残留的 data 行同样 join 成 payload 交付一次。注释写了两条理由——按 SSE 规范，最后一个事件即使没有尾随空行也应派发；这也覆盖"连接在事件中间掉线"的场景。冲刷同样尊重 `[DONE]`（:76-78）。

代价与收益：一个被截断的半帧 JSON 会交给上层解码，解码失败浮出为错误。错误比静默丢弃诚实——调用方由此知道流没有正常结束，而不是误以为服务器只发到这里。正常 EOF（无残留、无错误）**不产出错误**（:24-25 注释）——"结束"本身是合法状态，"结束得是否太早"由适配器的完整性判定负责（§1.2 第 5 条）。

### 2.3 错误如何浮出：三段机制配合

传输层感知中断有两个来源，浮出路径各有设计：

**读取错误（连接断、行超限）**。`scanner.Err()` 非空时 yield 一次 `("", err)` 并停止（:85-89）。若此刻 `ctx.Err() != nil`，优先产出 `ctx.Err()`——让"调用方主动取消"以取消身份浮出，而不是以连接被关后的 "use of closed response body" 这类噪音浮出。

**取消的及时性靠两段机制配合**。逐行循环里每行先做一次非阻塞 ctx 检查（:33-38），处理"读取仍在成功推进、但调用方已取消"的情况；但 scanner 阻塞在网络 read 上时，逐行检查永远走不到——所以 `DoStreamRequest` 起了一个看门狗 goroutine，`ctx.Done()` 时直接 `resp.Body.Close()`（llm/http.go:130-136），强行使阻塞中的 read 返回，控制权交回上一段。`Cleanup` 用 `sync.OnceFunc` 保证 close 与 `close(done)` 恰好执行一次（:140-143），早退、自然结束、取消三条路径共用。

### 2.4 为什么传输层只浮出、不重试

`DoStreamRequest` 的注释把契约写在门口："No retry logic is applied — streaming responses cannot be safely retried because partial state may already have been consumed"（llm/http.go:93）。传输层是**感知与浮出**的单位：它知道"字节流断了"，但它不知道已经交付了多少 chunk、消费者看到了什么、重试是否值得——这些知识在循环层。重试决策、退避、RetryEvent 记账全部住在 `chatWithRetryAndFallback`（agent/provider_call.go:13，由 agent/agent.go:466 每轮调用），其完整论证（重复交付、计费不可判定、装配状态、消费者早退四个理由）见 10-agent-loop.md §5.3，此处不重复。传输层的义务是：把错误以**可分类的形状**（`*llm.APIError` 或原样网络错误）如实交给上层，不吞、不改写——这是包装器守则的要求（docs/DESIGN.md:62）。

## 3. pre-stream 重试与 fallback 的传输层细节

### 3.1 两条浮出通道："直接返回"契约

`Provider.ChatStream` 有两条错误通道：同步返回值，和迭代器 yield 的错误。**只有前者进入循环层的重试**——"仅 ChatStream 直接返回的可重试 error 触发既有 retry；iterator yield 的 error（含零 chunk）不 retry"（docs/DESIGN.md:166；codex 侧复述于 llm/doc.go:10-11）。这条契约把"pre-stream"从时间概念精确成了**通道概念**：同样是"一个 chunk 都没交付"的失败，走哪条通道决定了它能不能被重试。

四个适配器在这个契约下做了不同的物理选择，值得如实记录：

- **openai**：把 `DoStreamRequest` 放进返回的迭代器里（llm/openai/adapter.go:211），建连与非 2xx 错误在**首次 range 时**才发生、以迭代错误浮出——遵守接口注释的惰性契约（llm/provider.go:27-29），由 `TestProvider_ChatStreamIsLazy` 钉死（llm/openai/adapter_test.go:99-120：range 之前请求数必须为 0）。代价：这类失败**不会**被循环层重试。
- **openairesponses**：同 openai，惰性（llm/openairesponses/stream.go:88 在返回的闭包内建连）。
- **codex**：同 openai，惰性（包注释直说 "the agent's outer retry loop does not retry lazy iterator errors"，llm/codex/adapter.go:3-5）。
- **glm**：在 `ChatStream` 函数体内就调 `DoStreamRequest`（llm/glm/adapter.go:311-319），建连错误从同步返回值浮出，**进入**循环层的重试循环。

这个不对称是当前代码的真实状态而非理想形态：同样是"连接没建起来"，glm 的会得到最多三次退避重试，openai 一族的会直接作为本轮错误浮出。DESIGN.md 只承诺前者的重试，所以两者都不违约；但写新适配器或排查"为什么 429 没有触发 RetryEvent"时必须知道这个差别。惰性换来的是"构造 ChatStream 永不失败、错误全部走流"的简单心智模型；eager 换来的是建连失败可重试。

### 3.2 重试条件：传输层如何生产"可分类的错误"

循环层的重试判据只有两个（agent/provider_call.go:66）：`IsRetryableError`（`*llm.APIError` 且 `Retryable()`）与 `IsNetworkError`。传输层的责任是让这两类判断有料可判：

- **非 2xx → `*llm.APIError`（叶子类型，无 Unwrap）**。`DoJSONRequest` 与 `DoStreamRequest` 都在状态码出界时构造它，附带状态码、（可能被截断的）错误体、以及解析好的 `Retry-After`（llm/http.go:59-64；流式路径 :115-127，读错误体失败时用 `errors.Join` 同时保住读取错误与 APIError，:124）。`Retryable()` = 非 `NonRetryable` 且（429 或 5xx）（llm/errors.go:21-23）；`NonRetryable` 是给"状态码看似可重试但语义上不可"的出口，目前唯一使用者是 codex 的配额分类（§5.4）。
- **网络错误保持原样、只包 `%w`**。`client.Do` 失败返回 `fmt.Errorf("http request: %w", err)`（llm/http.go:48-50；流式路径 :110-112）——包装但可穿透。`IsNetworkError` 靠 `errors.As` 识别 `net.Error` 的 Timeout/Temporary、`url.Error` 解包后的网络错误、以及 ECONNREFUSED/ECONNRESET/EPIPE 三个 syscall 哨兵（llm/retry.go:86-114）。显式排除三类：`context.Canceled` / `DeadlineExceeded`（取消绝不重试）、`ErrStreamingNotSupported`（那是降级信号，不是故障）、`*llm.APIError`（已有自己的分类）。测试矩阵在 llm/retry_test.go:42-96。

### 3.3 Retry-After 的完整旅程

服务器说了何时可以再试，传输层负责把这句话原样带到决策点：

```
HTTP 头 Retry-After
  → parseRetryAfter（llm/http.go:147：秒数与 HTTP-date 两种格式，溢出/过期归零）
  → APIError.RetryAfter 字段（:63 / :121）
  → WaitForRetry 优先采用（llm/retry.go:54-69：覆盖抖动退避，仍受 ctx deadline 截断）
```

退避算法本身（全抖动指数退避，上限 `DefaultMaxDelay`=120s，llm/retry.go:35-47）属于重试层，10-agent-loop.md §5.2 已讲。传输层的独有贡献是头解析；codex 在 transport.go:76-89 有一个几乎逐字相同的 `retryAfter` 拷贝——llm 侧的原函数未导出，导出它需要一次公共 API 决策，于是按复制纪律各持一份（§4）。

### 3.4 降级链的传输层视图

`ChatStream` 同步返回 `ErrStreamingNotSupported`（llm/provider.go:14）时，循环层在**同一次尝试内**降级到非流式 `Chat`（agent/provider_call.go:39-46），成功后 `synthesizeChunks` 把整条回复回放成合规 chunk 流（:49-56、:104）——降级不是重试、不消耗 attempt、不发 RetryEvent，语义论证见 10-agent-loop.md §5.4。传输层看到的是一条**完全不同的管道**：`Chat` 走 `DoJSONRequest`——整读（10 MB 封顶）、整解、一次返回（llm/openai/adapter.go:122），没有 SSE、没有看门狗、没有部分帧。这条管道的存在让"不支持流式的 provider"对循环完全透明；降级后若失败，错误与流式失败共享同一套 `APIError`/网络错误分类，回到同一个重试判据（agent/provider_call.go:46-47 赋值后落入公共路径）。

哨兵本身是传输契约的一部分：包装器不得吞掉它（docs/DESIGN.md:62——"尤其不得吞 ErrStreamingNotSupported，否则非流回退与重试分类失效"），`IsNetworkError` 显式把它排除在重试之外（llm/retry.go:93-95），保证"降级信号"与"重试信号"在类型层就分开。

### 3.5 错误体脱敏

`*llm.APIError.Body` 装着服务器原始响应体，可能包含账户标识、内部端点等敏感细节。它流向日志，不流向用户可见面：`SanitizeRetryReason` 把错误压成 "rate limited (429)" / "server error (500)" / "connection failed" 这类一句话理由（llm/retry.go:118-134），作为 `RetryEvent.Reason` 交付；函数注释写明"never exposes raw API response bodies — sensitive details go to slog only"。传输层负责**收集**原文（诊断需要），呈现层负责**不外泄**——两个职责因此可以各自演化。

## 4. 适配器复制纪律：为什么 glm/openai 的转换代码故意不抽象

### 4.1 重复的是什么

glm 与 openai 两个适配器各自维护着近乎逐字相同的转换助手：`convertMessages` / `convertMessage` / `convertStandardMessage` / `convertAssistant` / `convertToolResult` / `convertToolDefs` / `convertResponseMessage` / `convertUsage`（openai：llm/openai/adapter.go:307-495；glm：llm/glm/adapter.go:419-598）、小工具 `firstNonEmpty` / `encodeBase64` / `ptrToString`（openai :497/:506/:510；glm :580/:600/:604）、流载荷解析 `parseStreamPayload`（openai :253；glm :344），以及 §3.3 的 `retryAfter` 拷贝（codex 侧）。openairesponses 也不复用它们——它有自己的 `convertMessages`（typed items 语义，llm/openairesponses/convert.go:73）和自己的 `firstNonEmpty`（llm/openairesponses/stream.go:235）。

这是**成文的裁决**，不是懈怠：两级 AGENTS.md 的反模式清单钉着同一条——"Conversion helpers … duplicated near-verbatim in glm/ and openai/ BY DESIGN — change both, never extract a shared helper"（llm/AGENTS.md ANTI-PATTERNS；根 AGENTS.md 同条）。操作规则是：改语义时**两边都改**，永远不提取共享层。

### 4.2 为什么故意不抽象

重复的代码分两类，各有各的不抽象理由。

**协议方言类（convert\* 家族、parseStreamPayload）**。这些函数看似相同，实则是两种方言演化中的暂时重合，差异已经真实存在：glm 出站**剥掉所有** reasoning 块（"Silently strip — GLM doesn't accept reasoning in outbound messages"，llm/glm/adapter.go:456/:483），openai 却把 `ReasoningBlock` 保留进 `reasoning_content`（为 DeepSeek 风格的 thinking 模式，llm/openai/adapter.go:365-366）；glm 的 `finish_reason` 要经 `finishReasonError` 映射成 `APIError`（`network_error`→503、`model_context_window_exceeded`→400，llm/glm/adapter.go:402-417），openai 没有；glm 的 stop 序列最多一项（llm/glm/adapter.go:165-169）、usage 永远没有 reasoning tokens（:591）。一旦抽成公共层，每一次单侧协议漂移都要在公共层里长出开关参数——公共层从"共享"退化成"带 provider 分支的最低公分母"，比两份独立代码更难改也更难测。厂商协议独立漂移是常态而非例外，所以成本比较是：抽象省下一次拷贝，代价是从此每次漂移都要在公共层谈判；裁决选了前者。

**纯函数小工具类（firstNonEmpty、encodeBase64、ptrToString）**。这一类没有方言问题，抽象的唯一障碍是落点：放进 llm 包根就要成为公共 API 面（导出），或为它新建子包（新的边界决策）——为一个三行函数支付这个价格不值。它们的重复是"公共层准入门槛"的合理代价。

分界线其实画得很清楚：**协议无关的传输机制全部共享**——`DoJSONRequest` / `DoStreamRequest` / `ScanSSEEvents` / `APIError` / 重试原语住在 llm 包根，四个适配器都消费；**协议方言一律不共享**。所以复制纪律不是"llm 层反对复用"，而是"复用必须先证明协议无关"。

### 4.3 什么条件下应重新考虑合并

裁决附带诚实的边界——出现下列信号时，值得把"提取"重新放上桌面：

1. **同一 helper 出现第三个消费者且语义逐字一致**。openairesponses 的 `firstNonEmpty` 已经是第三份拷贝，是现有最强的信号；三份以上相同纯函数的维护漂移风险开始超过 API 面成本。
2. **重复本身造成过真实事故**：一侧修了 bug、另一侧忘改、且被测试漏过。届时"change both"这条社会性纪律已被证明不可靠，需要机械保证（共享，或交叉钉住两份实现的等价性测试）。
3. **厂商层面协议收敛**：若两个 API 的 wire 格式在相关字段上出现正式规范并对齐，"方言"前提消失，转换层就只剩纯函数。

任一信号出现之前维持现状；任何提取都必须同步改 DESIGN.md 与两级 AGENTS.md 的反模式条目（docs-first 纪律）。

## 5. 鉴权处理模式对比：bearer / JWT / OAuth

### 5.1 三种模式一览

| 维度 | bearer（openai / openairesponses） | JWT 自签（glm） | OAuth 订阅（codex） |
|---|---|---|---|
| 凭据 | 静态 API key | `{id}.{secret}` 复合 key | access + refresh token 对 |
| 构造期动作 | 无 | key 含 `.` 即选 JWT 模式（llm/glm/adapter.go:105-108） | 无 I/O，凭据问题延迟到调用（llm/codex/adapter.go:68-70） |
| 每请求开销 | 字符串拼接（llm/openai/adapter.go:72-75） | 缓存命中取 token，未命中 HMAC 签发（llm/glm/auth.go:84-101） | source 快照 + 至多一次 401 恢复（llm/codex/transport.go:18-74） |
| 过期处理 | 无（key 不过期） | 提前 5 分钟重签（llm/glm/auth.go:87） | Refresh 独占刷新 + single-flight（llm/codex/auth/refresh.go） |
| 附加头 | Authorization | Authorization | + `ChatGPT-Account-Id` / `Originator` / `User-Agent`（llm/codex/transport.go:34-37） |

### 5.2 bearer：把无状态留给传输层

openai 的 `apiHeaders` 是三种模式里最薄的：`Authorization: Bearer <key>`，一次 map 构造（llm/openai/adapter.go:72-75），Chat 与 ChatStream 共用（openairesponses 同款，llm/openairesponses/adapter.go:74-77）。key 的获取、轮换、过期全部是调用方的事——传输层不拥有凭据生命周期。这是"机制与策略分离"（docs/DESIGN.md:58）在鉴权上的形态：key 怎么来是策略，怎么带上请求是机制。

### 5.3 glm JWT：自签、嗅探与缓存

GLM（智谱）的鉴权有两种合法形态，同一个 `apiKey` 参数承载：不含点的 key 直接作 bearer；含点的 key 是 `{id}.{secret}` 复合形态，需要本地用 secret 对 `header.payload` 做 HS256 HMAC、拼出 JWT 再作为 bearer 发送（`generateToken`，llm/glm/auth.go:25-70；header 带 `sign_type: SIGN`，exp 与 timestamp 用毫秒）。选择方式是**构造期嗅探**：`strings.Contains(apiKey, ".")`（llm/glm/adapter.go:105-108）。

嗅探的代价被成文记录："bearer key with a dot WILL misauthenticate"（llm/AGENTS.md）——一个恰好含点的普通 bearer key 会被误判进 JWT 模式。为什么不做成显式选项？因为两种 key 形态在 GLM 侧本来就是同一发行渠道的两种格式，按内容分派让调用方零配置；误判面（合法 bearer key 恰好含点）在实践中不出现。这是一个"约定压倒显式"的局部让步，换来 `NewProvider(apiKey, model)` 接口保持两个参数的简单。

token 生命周期由 `tokenCache` 管理：`sync.Map` 按 key 缓存，有效期 3600 秒，**提前 5 分钟**视为过期重签（llm/glm/auth.go:84-101），避免把"刚签好就过期"的竞态发到线上。签发发生在首个请求的路径上（`apiHeaders` → `getToken`，llm/glm/adapter.go:129-137），Chat 与 ChatStream 因此都无感；签发失败从 `apiHeaders` 返回错误（:132-134）。测试通过 `timeNow` 包变量注入时钟（llm/glm/auth.go:14）——项目"test seams are package vars"惯例的实例。

### 5.4 codex：OAuth 订阅的四件套

codex 是四个适配器里唯一"凭据有生命周期"的：ChatGPT 订阅的 access token 会过期、refresh token 会被轮换。传输层因此多出一个鉴权子包（llm/codex/auth/），分四件：

1. **token 解析**（token.go）：`ParseToken` 只读**未验签**的 JWT claims（`exp`、`chatgpt_account_id`），供过期调度与账户路由；函数注释言明"does not authenticate the issuer, signature, or identity"（llm/codex/auth/token.go:13-17）。两处 account claim 冲突即拒绝。
2. **令牌端点交互**（endpoint.go）：`requestTokens` 带 30 秒硬超时（ctx 与 client 双保险，llm/codex/auth/endpoint.go:40），token 响应体同样有 1 MiB 上限（:17）。**关键设计**是对"结果不确定"的细分类：请求发出前失败（dial 阶段，靠 httptrace 的 GotConn 判定）标记 Temporary、可重试；发出后失败是 `unknown_outcome` + LoginRequired（:74-104）——refresh token 可能已在服务器侧轮换，盲目复用会烧掉凭据，所以宁可要求重新登录（docs/DESIGN.md:286-288）。
3. **刷新编排**（refresh.go）：`RefreshingSource` 把"到期判断、401 恢复、single-flight、持久化"收进一个串行化单元（llm/codex/auth/refresh.go:19-27）；`Token()` 只回最近发布的快照、不联网（:44-50），`Refresh()` 独占刷新，同批等待者共享同一航班的结果、不逐个重试（docs/DESIGN.md:284-285）；`Refresh(&used)` 只使与被拒 token 相同的那一代失效，迟到的旧 401 不强制刷新新代（docs/DESIGN.md:281-282）；新凭据**先 Persist 后发布**，保存失败保留 pending、只重试保存（docs/DESIGN.md:286-287）。
4. **每请求应用**（transport.go）：`openStream` 从 source 取 token，带上 `Authorization` + `ChatGPT-Account-Id` + `Originator` + `User-Agent`（llm/codex/transport.go:34-37），并核对账户一致性（token 换代即拒，:25-27）。**401 恰好恢复一次**：关闭响应体、`Refresh(&used)` 使被拒的一代表现失效、重发一次，再失败即终局（:56-61，docs/DESIGN.md:356）。错误分类叠加在这一层：服务器错误经 `classifyServerError` 分 Kind——配额（`usage_limit_reached` 等）→ `NonRetryable=true` 的 APIError 包进 KindQuota；401→auth；400/403/404/422→capability；其余原样返回 APIError（llm/codex/http_errors.go:12-38）；配额的 reset 时间从响应体解析进 `RetryAt`（:40-57，秒数或 RFC3339 两种形态）。

包注释把这套东西的边界说得干脆："Refresh owns authentication and persistence; Token is only a read-only snapshot. Interactive login and file storage belong to codexauth"（llm/doc.go:3-6）——交互式登录、浏览器流程与文件存储（`~/.go-agent/codex/auth.json`、0700/0600 权限、O_EXCL 锁）在 codexauth 卫星，内核侧只保留无交互依赖的机制。

### 5.5 为什么鉴权住在适配器而不是共享层

鉴权方言是协议方言的一部分：openai 的静态 key、glm 的 `id.secret`、codex 的 OAuth 轮换，三者的生命周期、失败模式、恢复动作没有一行可以共享。共享管道对此的立场是把接口收到最窄——`RequestConfig.Headers` 就是一个 `map[string]string`（llm/http.go:27），llm 包根本不知道"鉴权"这个概念，适配器在自己的 `apiHeaders()` 里完成一切。这与 §4 的分界线同构：传输机制（把头带到线上）共享，协议语义（头是什么、怎么来、怎么续命）不共享。每个适配器因此各自消化鉴权失败——glm 的 JWT 签发错误从 `apiHeaders` 浮出（llm/glm/adapter.go:132-134），codex 的刷新失败成为 `KindAuth` 错误（llm/codex/transport.go:19-21）——llm 包不需要理解其中任何一个。

## 交叉引用

- **10-agent-loop.md §5**：重试边界的完整论证（mid-stream 禁止重试的四个理由、fallback 链语义、退避配置与 RetryEvent/Retries 记账）——本文 §2.4/§3 只讲传输层如何感知与配合。
- **20-llm-model.md**：`Message` / `ContentBlock` / `Chunk` 密封联合、`ReasoningItem` 语义——传输层交付的载荷形状归它。
- **docs/DESIGN.md**：接口契约的权威文本。本文引用的冻结条目：惰性契约与包装器守则（:62-63）、"仅 ChatStream 直接返回的可重试 error 触发 retry"（:166）、codex 传输限额（:343）、401 一次恢复与惰性错误不重试（:356-358）、auth.Source 刷新契约（:279-288）。
- **00-overview.md**：四包切分与"内核零第三方依赖"守卫——HTTP/SSE 管道全部 stdlib 实现，是这条守卫的最前线。
