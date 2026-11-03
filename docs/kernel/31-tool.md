# 31 · 工具与 schema（tool 包）

> 内核文档集第 31 篇。本文只讲 `tool` 包的**接口与数据契约本体**：`Tool` 接口与 `ToolResult`、`Registry` 的并发模型与 `Info()` 冻结、`ParameterSchema` 递归模型与语义保真 codec，以及 `mcp` / `agenttool` 两个卫星在接缝上的组合面。
> 工具在循环里如何被调度、审批、并行结算、结果限长是执行侧视角，已在第 11 篇（11-agent-tools.md）讲透，本文不重复，只在交界处回指；整体设计意图与端到端场景见 docs/TOOL-DESIGN.md（对象读者在那里），本文补代码级视图；冻结契约原文以 docs/DESIGN.md 为准（工具热变更 P2-4 docs/DESIGN.md:423、Info 冻结 P2-5 docs/DESIGN.md:432、MCP 桥 P3-3），两者冲突时以 DESIGN.md 为准。
> 行号锚点基于 2026-10-29 工作区（基线 65fea54，含 provider 热换改动），与 11-agent-tools.md 同批。

## 1. Tool 接口与 ToolResult 数据契约

### 1.1 两面性是接口的全部

```go
// tool/tool.go:14
type Tool interface {
    Info() ToolInfo
    Execute(ctx context.Context, args json.RawMessage) (*ToolResult, error)
}
```

两个方法各面向一个受众。`Info()`（tool/tool.go:33 的 `ToolInfo`：`Name` / `Description` / `Parameters` / `RequiresApproval`）面向**模型**：直接映射为 provider 的 function-calling 声明，第 20 篇讲适配器如何把它写进请求体。`Execute()` 面向**运行时**：接收模型产出的原始 JSON，返回结果。`ToolInfo` 四个字段里唯一有行为语义的是 `RequiresApproval`——为 true 时循环必须先过审批回调，无回调则 fail-closed 拒绝执行（执行细节在第 11 篇 §2.7；MCP 桥的"默认全部审批"只是叠加在这之上的策略，见 §5.1）。

`args` 用 `json.RawMessage` 而不是 `map[string]any` 或泛型，三个理由：反序列化是每个工具自己的事（接缝不承担类型翻译成本）；工具想怎么严、怎么宽自己定；原始信息（字段顺序、重复键）一个字节不丢地交给工具。

`Info()` 有一条隐含契约：**纯读、无副作用、幂等**。这条契约历史上只写在文档里；P2-5 之后由注册流水线操作性强制——包内只有一处调用用户 `Info()`（§3.1），`List` 从不回调（§2.3），实现"不纯"也不会再影响注册表。执行侧 `Info()` 还有第二个调用点：并行路径的规划边界（第 11 篇 §2.2），那里拿到的是冻结后的视图，不是用户的 Info。

接口为什么只有两个方法、为什么不密封：接缝的全部扩展都以"实现这个接口"完成——mcp wrapper、agenttool 适配器、skills 分发器、用户函数，内核从未需要改接口。§5 是这条开放-封闭性质的三份证据。

### 1.2 ToolResult：两值 Status 与 Data 的可见半径

```go
// tool/tool.go:93
type ToolResult struct {
    Content string          `json:"content"`
    Data    json.RawMessage `json:"data,omitempty"`
    Status  ResultStatus    `json:"status,omitempty"`
}
```

`Status` 的全部取值恰好两个（tool/tool.go:86-91）：`ResultSuccess`（`""`）与 `ResultError`（`"error"`）。两个 json tag 带 `omitempty`，所以成功结果序列化后不含 status 字段。判定入口只有一个：`IsError()`（tool/tool.go:99）。

构造器三件套：

| 构造器 | 位置 | 语义 |
|---|---|---|
| `NewTextResult` | tool/tool.go:109 | 纯文本成功结果 |
| `NewDataResult` | tool/tool.go:116 | 文本 + 结构化数据；marshal 失败以 Go error 在**构造期**交还调用方，由调用方决定上抛还是改道 |
| `NewErrorResult` | tool/tool.go:101 | 软失败的唯一合法构造器；**禁止手写 Status 字符串**——Status 只有两值，手拼字符串拼错一个字母就静默变成"成功" |

`Data` 的可见半径要钉死：它只对**当次运行的 `ToolResultEvent` 消费者**可见；`DoneEvent.History` 与 Store 持久化只保留 `Content`。为什么：结构化数据可能很大——MCP 桥把 1 MiB 的结果信封放在 Data 里（§5.1）——不该膨胀持久化日志；历史需要的是模型可见的对话内容，那恰好就是 Content。持久化侧的对应契约在第 12 篇。

字段名是对外契约（结果会经事件与序列化路径外流），由 `TestToolResult_JSONRoundTrip`（tool/tool_test.go:246）钉死。

### 1.3 双通道的接口侧含义

接口注释即契约原文（tool/tool.go:25-28）：业务失败（坏入参、资源不存在……）返回 `ToolResult{Status: ResultError}` 喂回 LLM；Go error 只保留给应当中断循环的系统级故障。**分类责任在工具作者，内核不做猜测**——docs/DESIGN.md:62 把边界钉成双向禁令：软错误 ToolResult 不得提升为 Go error，Go error 不得降级为软结果。执行侧的 enforcement（worker 里非 nil error → 槽位 hardError、nil result → 软错误 "returned nil result"、TC/TR 配对在任何结局下完整）全在第 11 篇 §3，本文不重复。

**被否决的替代（接口层）：单通道 error + 循环层统一转软。** 转换点必须回答"哪些 error 该转、哪些不该"，这个分类恰恰是工具作者掌握的领域知识；接口层强制作者在签名上表态，内核不越权代答。完整论证在第 11 篇 §3.2。

schema 构造捷径三件（`NewParameterSchema` tool/tool.go:131、`Param` :139、`ParamEnum` :144）供外部构造 typed 子集；§4.2 的外部构造归一语义同样适用于它们。

## 2. Registry：并发模型与生命周期

### 2.1 数据形状：不可变条目 + 两层缓存

```go
// tool/registry.go:13
type registryEntry struct {
    tool       Tool     // live execution handle; Get returns it unchanged
    info       ToolInfo // frozen canonical: deep copy taken at registration
    paramsJSON []byte   // json.Marshal of the frozen Parameters
}
```

条目三字段在锁外的冻结流水线里一次写就、此后不可变（注释标为 invariant I4，tool/registry.go:10-12）。`Registry`（tool/registry.go:20）在此基础上加聚合层：`snapshot` 是按名升序的 `[]ToolInfo`，nil 表示脏、非 nil 空 slice 表示干净空表（I10），重建永远整数组替换、绝不原地编辑（I5）。`rebuilds` 计数器是测试缝，`TestListSnapshotLifecycle` 靠它断言重建次数（含 Apply 折叠为一次重建）。

### 2.2 锁纪律：用户代码与序列化都在锁外

写路径统一先走 `freezeForRegistration`（tool/registry.go:66）：**取锁前**完成校验 → 深拷贝冻结 → 序列化三步，三步都不依赖注册表状态。然后 `Register`（:79）/ `Replace`（:173）/ `Apply`（:197）各拿一次写锁，临界区内只有 map 操作加 `snapshot = nil`。

为什么校验在锁外（tool/registry.go:39-40 注释）：用户实现的 `Info()` 不可信——可能慢、可能 panic，绝不在持 `RWMutex` 期间调用。这也让 `Apply` 的"先全量校验、任一失败零变更"自然成立：任何失败都发生在任何锁与任何写入之前。

`Get`（tool/registry.go:98）拿读锁点查，返回**live 工具句柄**而非任何缓存副本（注释原话："The cache does not apply: execution needs the Tool itself, not its description"）。这是热变更的执行层锚点：运行中途 `Replace`/`Unregister` 后，下一次 `Get` 立刻看到新实现或未命中；可见性规则（默认 run 起点广播快照 vs `WithDynamicTools` 轮级刷新）与两个解析点的定义在第 11 篇 §6 和 docs/DESIGN.md:423（P2-4）。

`List`（tool/registry.go:110）读锁读 snapshot → 解锁；若脏则升级写锁重建（双检查）→ 解锁后才逐条深拷贝。重建（`rebuildSnapshotLocked` :134）只做排序与装配，无 codec、无用户代码。空注册表返回**非 nil 空 slice**（`make` 保证，`TestRegistry_List_Empty` 钉死）。

为什么 `RWMutex` 而不是 `sync.Map`：注册表规模小（几十个工具）、读写比不极端，RWMutex 语义清晰，且 `Replace`/`Apply` 这类复合原子操作必须在单一临界区内表达，sync.Map 做不到。并发正确性由 `-race -shuffle=on` 覆盖：`TestRegistry_ConcurrentAccess` / `TestRegistry_ConcurrentReadWrite` / `TestRegistry_Get_Concurrent`（tool/registry_test.go）、`TestRegistryConcurrentMutations`（tool/registry_mutation_test.go）、`TestRegistryConcurrentStressCache`（tool/registry_cache_test.go）。

### 2.3 List 为什么绝不回调用户代码

冻结之后 `List` 是纯数据操作：每次调用拿到逐条深拷贝（`deepCopyToolInfo` tool/registry.go:241——嵌套 map、slice、byte 数组、指针全不与注册表 alias，invariant I7），改返回值影响不了注册表，也影响不了其他 List 调用者（`TestRegistryListReturnsCopy`、`TestListDeepCopyIsolation`）。`TestListNoUserCodeUnderLock` 用带锁状态探针的工具钉住"锁内零用户代码"；`TestListInfoCallCount` 钉住注册之后 `Info()` 的调用次数为 0。

**被否决的形态（P2-5 之前的实况）：List 持读锁逐个调用 `t.Info()`。** 三个具体病灶见 §3.2——那正是冻结裁决要解决的东西。

### 2.4 生命周期方法一览

| 方法 | 位置 | 语义 |
|---|---|---|
| `Register` | tool/registry.go:79 | 冻结流水线 + 写锁插入；重名报错；校验失败零变更 |
| `MustRegister` | tool/registry.go:144 | Register 的 panic 版，用于"失败即程序错误"的装配场景 |
| `Unregister` | tool/registry.go:155 | 幂等注销，返回是否发生移除；不等待在飞 Execute；snapshot 只在真移除时置脏 |
| `Replace` | tool/registry.go:173 | 同名严格换实现，校验同 Register；名字不存在报错（无 upsert）；单锁内换入全新条目，观察者看不到名字缺席的中间态；需要刷新元数据也走它（§3.1） |
| `Apply` | tool/registry.go:197 | 批量原子：added 内部查重、与未被 removed 排除的现存名查重，任一失败零变更；removed 中未知名静默忽略（对终态、不对增量）；整个批次单锁落地、一次置脏 |

这些方法的冻结契约原文在 docs/DESIGN.md:423（P2-4）；MCP 桥 `Refresh` 把 `Apply` 当对账原语用（§5.1）。

## 3. Info() 冻结语义（P2-5）

裁决出处：docs/DESIGN.md:432，2026-10-28，方案 2a + D3。语义经外部调研定为业界共识：工具声明按注册时刻不可变。

### 3.1 冻结流水线

`freezeForRegistration`（tool/registry.go:66-76）是包内唯一调用用户 `Info()` 的地方（:62-65 注释，:63 原话 "the only user Info() call in the codebase"，实际调用点 :45）。流水线三步：

1. `validateForRegistration`（:41）：nil 检查、空名检查、`validateToolInfo` → `validateRegistrySchema` 结构校验（§4.6）；
2. `deepCopyToolInfo`（:241）：深拷贝冻结为注册表内部 canonical；
3. `json.Marshal(frozen.Parameters)`：把冻结 schema 的序列化字节缓存进条目。

产物是不可变 `registryEntry`。注册之后，工具实现修改自身状态、或 `Info()` 返回漂移值（含改名），都不影响已注册视图；要刷新元数据，用 `Replace` 整体换实现——原子完成、缓存全部重算。

### 3.2 解决了哪些竞态

P2-5 之前的 List 在读锁内逐个调用 `t.Info()`，有三个真实病灶，冻结逐一消除：

1. **慢 `Info()` 阻塞写者。** RWMutex 的写者要等全部读者退出；一个慢或阻塞的 `Info()` 会卡住所有 Register/Unregister。而 Registry 承诺运行中可变（agent 一边跑、卫星一边换工具），这条锁链直接打穿承诺。冻结后慢 `Info()` 只拖慢它自己的那一次 Register。
2. **`Info()` 重入 Registry 即自死锁。** RWMutex 不可重入。聚合型/分发型工具（典型：skills 的目录分发器）完全可能在 `Info()` 里再调 `List` 甚至 `Register` 补挂载——旧形态下必死锁。冻结后 List 无用户代码，重入退化为普通嵌套读。
3. **注册后漂移。** 工具实现持有可变状态时，同一个注册名在不同 `List` 时刻给出不同声明。并行路径在规划边界读一次 Info（第 11 篇 §2.2），模型可见声明与冻结执行句柄之间就会出现未经 `Replace` 的静默分歧；历史里记录的声明与下一次请求的声明对不上，回放与审计失去基准。冻结把"声明何时变"收敛为唯一入口：一次显式的 Registry 写操作。

外加一条防御性前置：注册时序列化失败即注册失败、带工具名显式报错，把理论上可能的请求期 marshal 失败移到注册期（docs/DESIGN.md:445-447；现有校验严格覆盖序列化前置条件，该分支正常不可达）。

### 3.3 两层读缓存与预留的快路径

条目层 `paramsJSON` 与 `List` 交付值的序列化**逐字节一致**（`TestFreezeFidelityBytes` 钉死）。目前包外没有消费者：openai/glm 适配器仍自行 `json.Marshal(t.Parameters)`（llm/openai/adapter.go:431、llm/glm/adapter.go:529），agent 循环在 run 起点与 `WithDynamicTools` 的每轮顶部调 `List`（agent/agent.go:389 / :412）。这份缓存是为后续"适配器直接取装配字节"的快路径预置的——语义（逐字节一致）先钉死，优化后置。

测试钉子汇总：`TestListFrozenAtRegistration`（漂移不影响视图）、`TestListSnapshotLifecycle`（置脏/惰性重建/Apply 折叠）、`TestRegistryConcurrentStressCache`（并发读写下的缓存一致性）、`TestListInfoCallCount`（注册后零 Info 调用）。

## 4. ParameterSchema：递归模型与语义保真 codec

### 4.1 为什么不是 map[string]any

工具参数声明必然经 Go 类型往返（Registry 冻结 → 适配器序列化 → 请求体）。raw map 往返有四种损失：**零值歧义**（`"type":""` 与 type 缺失是两个不同的 schema，map 分不出来）；**重复键静默丢失**；**数值 token 保真无从谈起**；**没有结构检查面**——环与字段冲突在 map 里不可见。所以是递归类型模型：`ParameterSchema`（tool/tool.go:49）为根（必为 object 形状），`Property`（tool/tool.go:66）为递归节点（`Items *Property`、`Properties map[string]Property`、`AnyOf/OneOf/AllOf []*Property`、`Ref`、`BooleanSchema`）。typed 子集覆盖 function-calling 的高频词汇，其余一切交给 `Keywords` carrier（`SchemaKeyword{Name, Value}`，tool/tool.go:42）按原文携带。

### 4.2 presence state：零值歧义的消除

未导出标志（`typeAbsent`、`propertiesInactive`、`descriptionPresent`/`descriptionAbsent`、`refPresent`、`decoded`，tool/tool.go:62 / :83）逐成员记录"出现/未出现"。两类来源一套规则：

**解码来源（`UnmarshalJSON`）。** root（tool/schema_unmarshal.go:9）重置为 `typeAbsent:true`，输入必须是 JSON object；`Property`（:78）先试裸布尔 schema（`true`/`false` → `BooleanSchema`），否则同 root 逐成员投影。关键归一：

- `{"type":["null","string"]}` 或 `["string","null"]` → `Type:"string"` + `Nullable:true`（`schemaType`，tool/schema_decode.go:49；仅两元素且一侧为 null，其余形态落 carrier）；
- `properties:null` → `Properties=nil` 且**不**置 inactive（null-first 归一）；properties 为非对象 → 整个成员落 carrier 且 `propertiesInactive:true`（此后注册校验的 typed 子集检查整层跳过，§4.6）；
- `description:""` 与 description 缺失由 presence 标志区分；enum 空数组落 carrier（空 enum 语义可疑，不如原文保留）；`additionalProperties` 的布尔/模式二形（`decodeAdditional`，tool/schema_decode.go:158）分别落 `*bool` 与 `*Property`。

**外部构造来源（直接字面量构造）。** `decoded=false` 走归一语义：指针非 nil = 出现、`Ref` 非空 = 出现、空 `Ref` = 缺失（对所有可外部构造的值统一，不搞特例）；`typeActive()`/`descriptionActive()`（tool/schema_presence.go:10 / :14）决定 type/description 是否输出。`TestSchemaExternalKeyedAPI`（tool/schema_external_test.go:18）钉住每个形态的确定输出，例如裸 `Property{Type:"string"}` 序列化为 `{"type":"string","description":""}`——外部构造的最小 string 属性带显式空 description，投影确定。

### 4.3 marshal：先校验，typed 字段先出，carrier 按名排序

`MarshalJSON`（tool/schema_marshal.go:10）先 `validateSchemaGraph` 再输出；`Property.MarshalJSON`（:18）把自己包进合成 root 走同一校验。输出规范：typed 字段按固定次序，然后 `Keywords` 副本按 Name 排序（`marshalObject` :183-186）；properties map 按 name 排序（`marshalProperties` :152）；required/enum 保 slice 原序；`Nullable` 写回 `["null", T]`（:58）。

关键词按名排序是**无损规范化**：JSON object 本身无序，且重名关键词在注册校验里直接拒绝（§4.6）——不存在"顺序承载语义又被排序丢掉"的窗口。

### 4.4 unmarshal：认得出才进 typed 字段，认不出整体进 carrier

每个成员两条路：形状严格匹配某个 typed 字段 → 投影进去；否则原文变成 `SchemaKeyword{Name, 原始 bytes}`。未知词汇**不报错**——carrier 就是完整词汇的兜底，这是"semantic fidelity"的核心承诺：codec 不理解 `format`/`minimum`/`pattern`……但一个字节都不丢。

解码卫生：`schemaObject`（tool/schema_decode.go:19）拒绝多 JSON 值与尾随内容；关键词 `Value` 内部的重复键由 `validJSONValue`/`scanJSONValue`（tool/schema_validate.go:189）递归拒绝——carrier 自己的 raw-JSON 路径不留脏东西。

### 4.5 环检测：fail closed，绝不挂起

JSON 文本解码天然无指针环（解码产物是树）；环只能来自程序构造——共享 `*Property`、共享 properties map。`encoding/json` 对环没有任何保护，自定义 `MarshalJSON` 更是全权自理：朴素递归 marshal 会栈溢出。`validateSchemaGraph`（tool/schema_validate.go:16）用 active-stack 检测器（`maps` 按 map 指针身份、`ptrs` 按 `*Property` 身份，入栈登记、出栈删除）检出任意环即报错。

两个出口共享同一个检测器：`Register` 的注册校验（`validateRegistrySchema` :71）与 `MarshalJSON` 的前置校验。所以无论从哪条路出闸，环都是带路径的显式错误（"properties cycle to ..." / "property cycle to ..."），绝不挂起、绝不溢出。`TestSchemaCodec_CyclesAndDAG` 断言环报错、DAG 通过。

### 4.6 Registry 只做 typed graph 结构检查，从不解释 carrier

`validateRegistrySchema`（tool/schema_validate.go:71）是两层：

1. **图结构层**（对整棵树，含对 carrier 的存在性约束）：组合环、`additionalProperties` 双形冲突、布尔 schema 与其他字段冲突（`booleanConflict`，tool/schema_presence.go:3）、关键词重名、关键词与 typed 字段同名冲突、关键词 Value 必须是单个合法 JSON 值（`keywords` 检查 :164）。
2. **typed 子集语义层**（只走 typed 边，且 `propertiesInactive` 时整层跳过）：required ⊆ properties、typed 属性 type 非空（`validateTypedProperties` :81 + `validateNestedTypedProperties` :98，递归覆盖 items、AP-schema、组合子）。

carrier 的 JSON Schema 语义**永不被解读**：`{"properties":[],"required":["missing"]}` 里两个成员都落了 carrier，注册通过（`TestRegistry_CarriedPropertiesSkipRequiredSubset` 钉住）；同样的 required-not-in-properties 出现在 typed 字段里就是注册错误，嵌套层也查（`TestRegistry_NestedRequiredIsValidated`）。

为什么止步于此：解释 carrier 语义等于实现一个 JSON Schema 校验器——巨大的第三方面积，与内核零依赖冲突；且误杀合法 schema 的代价大于放过（schema 本来就不校验参数：模型的 args 原样直达 `Execute`，仓库内不存在参数校验，理由在 docs/TOOL-DESIGN.md §5.3）。schema 是给模型的说明书，不是闸门；Registry 只保证说明书本身结构完好。

### 4.7 为什么禁止 whole-schema raw escape hatch

docs/DESIGN.md:50 与 :69 明文禁止，四条原因：

1. **两种表示必然分歧。** 冻结 canonical 必须唯一。一旦允许"整个 schema 用 raw bytes 绕过"，同一工具可同时携带 typed 视图与 raw 视图，冻结字节、深拷贝、presence 重建全部失去单一事实源。
2. **结构检查失去入口。** raw bytes 只能查 JSON 语法；环、双形冲突、关键词冲突在图层面不可见，会推迟到适配器 marshal（§4.5 的 fail-closed 被绕开）甚至 provider 请求期才爆。
3. **presence/carrier 语义被绕开。** raw 通道恰恰重新引入 typed 模型要消除的四种损失（§4.1），而且是静默的。
4. **不给适配器留降级路。** docs/DESIGN.md:69："adapter 不得静默降级 schema"。表示不了的 schema 应在边界 fail closed——MCP 桥对不可表示 schema 是整体 Connect 失败、零注册（§5.1）——而不是降级成 raw 直通。未知名词汇的逃生门已经存在：carrier 本身就是逐关键词、经校验、规范排序的逃生门，不需要 schema 级的第二个。

## 5. 组合面：mcpbridge 与 agenttool

接缝的验收标准是卫星零改内核完成组合（docs/DESIGN.md:50："不改 Tool interface"）。本节只讲两个卫星与 tool 数据契约的**交界**；各自的完整契约在 docs/DESIGN.md P3 节与各自 README。

### 5.1 mcp 桥：SDK map → canonical bytes → UnmarshalJSON 重建 presence

SDK 把远端 schema 解码为 `map[string]any`（`InputSchema`），重复键、出现顺序、精确数值 token 已在 SDK 边界丢失——保真上限就是 SDK decode surface（docs/DESIGN.md:69、mcp/README.md）。`projectSchema`（mcp/schema.go:62-78）三步：

1. 断言 decoded-object root（非对象 → `SchemaNotRepresentableError`）；
2. `json.Marshal(map)`——Go 的 map 序列化按键排序，产物是 canonical bytes；
3. `ParameterSchema.UnmarshalJSON` 重建 typed 图 + presence state + carrier。

表示不了（root 非对象、命名、marshal/projection 结构错误）→ `Connect` 整体失败、**零注册**（`prepareTools` 累积全部错误后 `sortedErrors` 统一上抛，mcp/bridge.go:127）——这是 §4.7 第 4 条"不降级、fail closed"的实例。

wrapper 的 `Info()`（mcp/bridge.go:225）返回构造时存好的值——卫星本来就守约；P2-5 之后 Registry 不再依赖任何卫星的自觉。桥的其余契约（命名规则 `ns__name`、审批默认全开、Data 信封与 1 MiB 预算、Close 排空）在 docs/DESIGN.md P3-3 与 mcp/README.md，本文不展开。

### 5.2 agenttool：固定 input:string、单次 Run、只回传最终文本

`New`（agenttool/agenttool.go:55）把一个预构造的子 Agent 包成 Tool。schema 固定为 required `input:string`，用 builder 搭（:56-60）。`Execute`（:74-101）逐条：

- 入参 `{input}`：用 `*string` 区分缺失/null 与空串；非法入参 → 软错误；
- 单槽 gate channel（:80-86）：每个 adapter 自己串行，取 gate 时 ctx 感知（取消 → Go error 硬上抛）；同一 child 的多个 adapter 不互锁，共享协作者（provider、store、registry）的并发安全归调用方；
- 每次执行无状态：只调一次子 `Run`（`Config.RunOptions` 可带 `agent.WithThread("")`，让每次 Execute 各得一条全新 durable thread）；
- 截断（`Truncated`）→ 软错误；子 Run/ctx 错误 → Go error 以 `%w` 上抛；
- 只按序提取 value-form `TextBlock` 拼成最终文本；`Data` 为 nil；**子事件一律不转发**，父层只见一个 `ToolResult`。

为什么是这个形状：委托边界是子 agent 的 prompt 而不是类型化 API——父模型用自然语言组任务，schema 面越小委托语义越稳定；只回传最终文本让父历史保持紧凑，不会被子 agent 的中间事件淹没；无状态单次 Run 让并行的父调用天然安全（gate 内无跨调用状态）。

### 5.3 第三个消费者：agenttest 回放

录制/回放的 v2/v3 格式对 parameters 走同一个 `UnmarshalJSON` 重建 presence（agenttest/wire_tool_decode.go:37；docs/DESIGN.md:421）。codec"不解释、只保真"的性质让它成为三处卫星共用的唯一重建入口——这是 §4 设计换来的复用面。

## 6. 测试即契约

tool 包的语义按测试文件分组钉死（行号从略，全部在 tool/ 下）：

| 测试组 | 钉住的契约 |
|---|---|
| tool_test.go | ToolResult 构造器三件套、JSON 往返、Status 两值世界、builder 助手 |
| registry_test.go | Register/Get/List 基础语义、排序、空表非 nil、重名/空名/nil 工具、并发读写 |
| registry_mutation_test.go | Unregister 幂等、Replace 无 upsert、Apply 原子零变更、List 返回副本 |
| registry_cache_test.go | P2-5 全套：锁内零用户代码、注册后零 Info 调用、冻结视图、快照生命周期、深拷贝隔离、冻结字节一致、并发压力 |
| schema_codec_test.go | codec 往返：legacy 字节、归一与 presence、carrier/组合/布尔 schema、环 fail-closed、结构错误 |
| schema_fidelity_test.go | 完整保真向量：root/property 归一与回退、additionalProperties、大数、显式空 `$ref` 拒绝 |
| schema_external_test.go | 外部构造 keyed API 的确定输出（§4.2） |
| schema_registry_test.go | 注册期结构校验的边界：递归合法、结构失败、carrier 跳过 typed 检查、嵌套 required 受检 |

## 7. 锚点速查

| 符号 | 位置 | 职责 |
|---|---|---|
| `Tool` 接口 | tool/tool.go:14 | Info + Execute 双面接缝；接口注释即软失败契约 |
| `ToolInfo` / `ToolResult` | tool/tool.go:33 / :93 | 工具身份声明 / 结果数据契约（Content·Data·Status） |
| `NewErrorResult` / `NewTextResult` / `NewDataResult` | tool/tool.go:101 / :109 / :116 | 结果构造器三件套 |
| `ParameterSchema` / `Property` / `SchemaKeyword` | tool/tool.go:49 / :66 / :42 | 递归 schema 模型 + 关键词载体 |
| `Registry` / `registryEntry` | tool/registry.go:20 / :13 | RWMutex 注册表 / 不可变冻结条目（I4） |
| `freezeForRegistration` | tool/registry.go:66 | 包内唯一用户 Info() 调用点；校验→冻结→序列化流水线 |
| `Register` / `MustRegister` / `Unregister` / `Replace` / `Apply` | tool/registry.go:79 / :144 / :155 / :173 / :197 | 生命周期五件套（P2-4） |
| `Get` / `List` | tool/registry.go:98 / :110 | live 句柄活查 / 排序冻结深拷贝快照 |
| `deepCopyToolInfo` | tool/registry.go:241 | 全隔离深拷贝（I7） |
| `UnmarshalJSON`（root / Property） | tool/schema_unmarshal.go:9 / :78 | 逐成员投影 + presence 重建 + carrier 兜底 |
| `MarshalJSON` / `marshalObject` | tool/schema_marshal.go:10 / :183 | 先校验后输出；typed 先出、关键词按名排序 |
| `validateSchemaGraph` / `validateRegistrySchema` | tool/schema_validate.go:16 / :71 | 环检测 fail-closed / 注册期两层校验（图结构 + typed 子集） |
| `schemaType` / `decodeAdditional` / `schemaObject` | tool/schema_decode.go:49 / :158 / :19 | 解码形状判定与入口卫生 |
| `typeActive` / `descriptionActive` / `booleanConflict` | tool/schema_presence.go:10 / :14 / :3 | 外部构造归一与布尔冲突判定 |
| `Connect` / `prepareTools` / `remoteTool` | mcp/bridge.go:47 / :127 / :217 | 桥入口 / 预备与 fail-closed / wrapper |
| `projectSchema` | mcp/schema.go:62 | SDK map → canonical bytes → UnmarshalJSON |
| `New` / `adapter.Execute` | agenttool/agenttool.go:55 / :74 | 子 agent 适配器 / 单次 Run 只回传最终文本 |
