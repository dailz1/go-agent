# 默认模型的未声明窗口警告归因设计

> 状态：提案，issue #23，2026-11-21。
> 对象读者：实现和评审 llm、agent 与 modelmeta 的开发者。
> 本文只设计诊断名称的补充。现有契约以 docs/DESIGN.md 为准；实施时先应用第 6 节增补，再修改代码。

## 1. 问题与目标

宿主构造 GLM Provider 时指定了默认模型，却没有传 `llm.WithModel`，也没有登记这个模型的窗口。请求会使用构造默认模型，警告却记录 `model=""`。宿主看到警告后，无法直接知道应该补哪个模型的窗口资料。

根因是窗口查询使用的模型选择值与适配器内部的构造默认值属于两个层次。空选择值有明确含义，内核不能为了补日志而把它替换成模型名。建议新增独立可选接口，让 Provider 报告诊断用的默认名称；只在未声明窗口警告的 `model` 属性和去重键中使用。

以下依据来自本次读取的工作区源码，行号为设计时位置：

| 已核对事实 | 源码依据 |
|---|---|
| 窗口元数据接口可选，空选择值原样传入，模型名匹配归实现方 | `docs/DESIGN.md:95`；`llm/context_window.go:8-24` |
| 当轮 options 已冻结，随后计算窗口，再进入压缩和主请求 | `agent/agent.go:534-551` |
| 未声明时把原始选择值送入警告，键为 provider 加分隔符再加 model | `agent/agent.go:818-853` |
| 去重表和锁属于 Agent 实例，在 New 中初始化 | `agent/agent.go:185-186`、`:366` |
| GLM、OpenAI 请求优先使用显式模型，否则使用 p.model | `llm/glm/adapter.go:144-150`；`llm/openai/adapter.go:86-91` |
| Responses 构造请求时同样回退到传入的 defaultModel | `llm/openairesponses/convert.go:272-278` |
| Codex 使用 p.model，允许调用选项覆盖；空模型在本地准备阶段报错 | `llm/codex/adapter.go:90-103` |
| 四个适配器的 ContextWindow 都先把空值解析为 p.model | 四包各自的 `context_window.go:19-25` |
| WithContextWindows 实际包装时只暴露 Provider 和精确窗口表 | `llm/context_window.go:35-79` |
| Guard 嵌入 llm.Provider，另行实现窗口查询，其他能力不会自动透出 | `modelmeta/provider.go:11-59` |

验收目标：没有显式模型、没有有效窗口声明且 Provider 能报告默认名称时，警告能显示该名称，并按该名称去重。查询参数、窗口结果、模型请求和持久记录保持原有语义。

## 2. 非目标

- 不扩展 `llm.Provider` 或 `llm.ContextWindowProvider` 的方法集。
- 不用诊断名称查询窗口、补写 `WithModel`、构造请求或建立用量身份。
- 不维护默认模型目录，不查询远端，不推断别名或快照家族。
- 不改变窗口计算、压缩、超长恢复、请求校验和重试规则。
- 不改 Store、事件、线程配置的格式，也不涉及 harness。
- 不建立包装器通用能力转发框架，不提取四个适配器的共享 helper。

## 3. 接口设计

### 3.1 独立诊断能力

建议在 `llm/context_window.go` 定义：

```go
// DefaultModelReporter optionally reports a provider's default model name
// for diagnostics when no model is configured.
// An empty result means the name is unavailable.
// The agent uses it only for undeclared-window warning attribution,
// never for window queries, request construction, or request identity.
// Implementations must be safe for concurrent calls and perform no I/O.
type DefaultModelReporter interface {
	DefaultModelName() string
}
```

这是普通结构接口，不嵌入 Provider、ContextWindowProvider，也没有私有标记方法。第三方 Provider 可以独立实现它。

推荐 `DefaultModelReporter`，因为 Reporter 表明这里只报告信息。`DefaultModelProvider` 容易让人理解成提供请求模型的通用能力；`ModelDefaultReporter` 的词序不如前者直接。方法名 `DefaultModelName` 说明返回值是名称，不是完整模型配置。

推荐返回单个 `string`。本任务只区分“有名称”和“无法报告名称”，空字符串足够表达后者。`(string, bool)` 可以区分“已知没有默认模型”和“名称未知”，但两者在本任务中都必须保留 `model=""`，增加状态没有用途。

四个仓内适配器分别增加同形方法，直接返回各自的 `p.model`。不新增字段、不归一名称、不检查窗口表，也不推迟到请求时解析。构造默认值为空时原样返回空字符串。

### 3.2 内核集成

查询诊断能力的位置放在 `roundContextWindow` 的未声明分支，紧邻现有警告调用。下列为函数级修改草图，省略号表示保留原函数已有部分：

```diff
 func (a *Agent) roundContextWindow(prov llm.Provider, opts []llm.Option) int {
     ...
     model := llm.ApplyOptions(opts).Model
     ...
     if cp, isCP := prov.(llm.ContextWindowProvider); isCP {
         declared, ok = cp.ContextWindow(model)
     }
     ...
+    warningModel := model
+    if warningModel == "" {
+        if reporter, ok := prov.(llm.DefaultModelReporter); ok {
+            warningModel = reporter.DefaultModelName()
+        }
+    }
-    a.warnUndeclaredWindow(prov.Name(), model)
+    a.warnUndeclaredWindow(prov.Name(), warningModel)
     return a.contextWindowTokens
 }
```

推荐保留 `warnUndeclaredWindow(providerName, model string)` 的签名及实现。传入值已经是最终警告名称，因此现有 `providerName + "\x00" + model` 自动同时满足日志与去重要求。无需再传 Provider、增加缓存或新增锁。

把能力查询放在警告 helper 内，可以集中日志逻辑，却需要扩大 helper 的输入职责。放在这里更直接：窗口查询先结束，有有效声明就提前返回；只有真正需要警告时才查询诊断名称。

`DefaultModelName` 在 `warnMu` 加锁前调用。锁继续只保护去重表；Provider 方法和日志输出都不在锁内执行。每个未声明且选择值为空的轮次至多查询一次，不缓存结果。重复轮次仍查询名称，才能正确处理不同 Provider 实例的默认值。

### 3.3 Guard 转发

建议给现有 `guardedProvider` 增加显式方法，不改变 Guard 构造和返回路径：

```go
func (p guardedProvider) DefaultModelName() string {
	if reporter, ok := p.Provider.(llm.DefaultModelReporter); ok {
		return reporter.DefaultModelName()
	}
	return ""
}
```

Guard 因而始终满足新接口，底层没有能力时返回空字符串。这个可观察的方法集变化需要明确记录；它不表示 Guard 推断出了默认模型。

另一方案是仅在底层支持时返回一个额外包装类型，精确保留能力是否存在。该方案多一个类型和构造分支，但内核对“接口不存在”和“返回空字符串”的处理完全一致。本提案推荐上面的显式委托，避免为没有行为差异的状态增加包装层。

报告名称不读取目录时间，也不受目录过期影响。目录过期正是需要警告的场景，不能同时把用于解释警告的名称抹掉。

## 4. 语义规范

1. **选择值与警告名称分开。** 选择值仍取 `llm.ApplyOptions(opts).Model`。窗口查询只接收这个原始值；查询次数和结果判定不变。诊断方法的结果只进入警告属性和去重键。
2. **非空选择值优先。** 显式配置模型时原样显示该名称，不调用新方法。即使报告值与显式值不同，也不参与本轮处理。
3. **空返回与缺失退化。** 选择值为空，且接口缺失或返回空字符串时，继续记录 `model=""`。不新增错误、警告类别或请求阻断。
4. **有效声明不报告名称。** `(tokens > 0, true)` 按现有规则计算 `min(C, tokens)`，不调用新方法，不产生未声明窗口警告。`false` 或非正数仍走回退。
5. **能力相互独立。** Provider 只有 DefaultModelReporter、没有 ContextWindowProvider 时，窗口仍未声明；空选择值的警告仍可使用报告名称。新接口不意味着已经声明窗口。
6. **不做第二次查询。** 空选择值查询未声明后，即使报告的名称存在于另一条窗口映射中，也不得再调用 `ContextWindow(name)`。警告显示这个名字，窗口仍回退到 C。
7. **按 Agent 实例去重。** 键继续使用 `providerName + "\x00" + warningModel`，同一 Agent 的所有运行和线程共享该表。保留现有分隔方式和锁，不增加进程级表。现有 helper 注释中的 “per process” 应改成 “per Agent”，以符合实际字段归属。
8. **混合轮次按最终名称处理。** 同一 provider，默认模型 A 与后来显式模型 B 不同，分别警告一次；后来显式选择 A，则与默认 A 共用一个键。显式与默认不是额外的去重维度。同名 provider 的不同实例若报告不同默认名称，也分别计数。
9. **热切换使用当轮 Provider。** 下一轮配置生效后，依据该轮已选中的 Provider 和 options 计算警告名称。更换 provider 名称形成不同键；切回已有 provider 和模型组合不重复警告。名称不写入线程配置或持久记录。
10. **名称原样使用。** 内核不 trim、不改大小写、不解释别名。报告值应反映实现自己的默认选择；这不保证模型有效、可调用或名称对应最新快照。
11. **WithContextWindows 保持精确表语义。** 存在正数登记项而实际形成包装时，不新增或转发 DefaultModelReporter。底层即使支持该接口，外层也继续只按原始选择值查表；宿主登记字面量 `""` 时会正常命中，无警告。若只登记名字 A，却以空选择值查询，仍未声明并警告 `model=""`。
12. **无实际包装时保留原对象。** WithContextWindows 的 nil、空表或全部非正数表返回原 Provider；原对象已有的诊断能力自然保留。“包装后不转发”仅指实际创建 contextWindowWrapper 的路径。
13. **Guard 保留诊断信息。** 新鲜、过期或时钟早于目录时间时，Guard 均原样转发报告名称；只有窗口查询受年龄规则限制。底层不支持时返回空字符串；若底层已经被 WithContextWindows 实际包装，Guard 不能越过该层恢复丢失的能力。
14. **Codex 空默认值保持失败路径。** 未登记空键窗口且没有显式模型时，先按原行为警告 `model=""`，之后请求准备报告 `model_required`。本提案不提前校验、不补模型。该错误以 endpoint 配置有效、且流程已到达请求准备为前提；endpoint 错误或更早的预算失败仍可先结束运行。
15. **报告方法的执行约束。** 实现须支持并发调用，无 I/O，不修改请求状态；仓内实现只读构造字段。内核不为第三方实现增加 panic 恢复、重试或串行化。

## 5. 各包改动点

以下均为后续实施清单，本次只新建本文：

- `docs/DESIGN.md`：第一步应用第 6 节文本，放在 P1 的 issue #17 条目之后，保留原接口和规则。
- `llm/context_window.go`：定义独立接口和用途注释；补充 WithContextWindows 只保留 Provider 和窗口能力的说明，并注明空表返回原对象的例外。原 ContextWindowProvider 的空选择值说明保持原意，可增加指向新接口的说明。
- `llm/openai/context_window.go`：新增直接返回 p.model 的方法。
- `llm/glm/context_window.go`：独立增加同形方法，不提取 helper。
- `llm/openairesponses/context_window.go`：独立增加同形方法。
- `llm/codex/context_window.go`：独立增加同形方法，空值原样返回。
- 四包各自的 `context_window_test.go`：每包增加一个表驱动测试，覆盖普通名称和空构造模型。
- `agent/agent.go`：按第 3.2 节修改未声明分支；纠正警告 helper 的实例级去重注释。保持 map、锁和 slog 属性键不变。
- `agent/provider_window_test.go`：增加局部手写双能力、单能力及无能力替身，记录查询参数与调用次数。不要让现有 windowedProvider 全部自动获得新能力，以免旧用例失去缺失能力的覆盖。
- `llm/context_window_test.go`：覆盖底层带报告能力时的实际包装和原对象返回两条路径。
- `modelmeta/provider.go`：按第 3.3 节显式委托；修正 “Only llm.Provider and llm.ContextWindowProvider are preserved” 的注释。
- `modelmeta/provider_test.go`：增加名称转发与空返回测试，保留已有年龄、请求委托和流式不预读测试。
- `modelmeta/README.md:98`：实施时同步更正“仅保证两个接口”的描述，说明新接口及空返回退化。它直接描述 Guard 方法集，继续保留原句会与代码冲突。
- `agenttest/`：不改。对该目录检索 ContextWindow 没有命中；这些公共替身没有构造默认模型或窗口契约。本任务用 agent 包内替身即可，无需扩大其 API。
- 根目录 `README.md`、`README.zh-CN.md`：不改。该变更不涉及 `scripts/check-readme-contract.sh` 检查的模块路径、Go 版本、安装示例、入口或链接；现有检查继续执行。
- 适配器文档：以四个新增方法的 GoDoc 说明只读诊断用途，无需再修改既有专项设计文档。`store/`、`tool/` 和 harness 不改。

实现不新增任何依赖，内核闭包继续只使用标准库。本文不要求修改 AGENTS.md 或已有专项设计文档。

## 6. DESIGN.md 增补文本

实施的第一步是在 P1「模型上下文窗口声明与有效窗口」条目之后追加以下文字，不改写 issue #17 原条目：

```markdown
- 默认模型的未声明窗口警告归因（issue #23）：`llm` 新增独立可选接口 `DefaultModelReporter interface { DefaultModelName() string }`，不扩展 `Provider` 或 `ContextWindowProvider`。该接口只报告未显式配置模型时供诊断使用的默认模型名称；空返回表示无法报告名称。实现须支持并发调用且不执行 I/O。四个仓内适配器分别返回各自构造默认模型，允许为空，不做名称归一。
  - 当轮未取得有效窗口声明且模型选择值为空时，内核可从当轮 Provider 查询该接口，以返回名称填入未声明窗口 Warn 的 `model` 属性及去重键；接口缺失或返回空字符串时，保留 `model=""`。选择值非空或已取得有效声明时，不查询该接口。诊断能力与窗口能力相互独立，没有窗口能力的 Provider 也可报告诊断名称。
  - 原始模型选择值仍原样传入 `ContextWindow`。诊断名称不得用于再次查询窗口、构造请求、修改 options、用量身份或持久记录；窗口和请求各自沿用既有默认模型解析。警告按同一 Agent 实例内的 provider 名称与最终警告模型名称组合去重；默认 A 与显式 A 共用一项，默认 A 与显式 B 分别计数。
  - `modelmeta.Catalog.Guard` 显式转发此诊断能力，目录过期不影响名称报告；底层不支持时返回空字符串。`llm.WithContextWindows` 实际创建的包装器仅保留 Provider 与精确窗口查询能力，不转发诊断能力，字面量空键的声明语义不变；空表或全部过滤后直接返回原 Provider 的路径保持原对象能力。其他包装器未转发时按接口缺失退化。
```

## 7. 测试计划

测试全部使用标准库 `testing`，命名子测试、表驱动、包内白盒，不请求真实 API。以下是实施验收计划，本次文档任务未执行这些测试。

### 7.1 内核查询与日志

复用 `agent/agent_truncate_test.go:369-397` 的 `truncationLogCapture`，用 `slog.New(capture)` 传入 Agent。直接调用 roundContextWindow 隔离其他日志，检查记录数和 `attrs` 中 `provider`、`model`、`window_tokens` 的值与类型；不匹配 message、时间戳或整段序列化日志。

取 C=8192，默认报告名 A，显式模型 B。带窗口的替身记录每次收到的参数，诊断替身统计调用次数。缺少窗口接口的用例使用真正不实现该方法的类型，不用“实现后返回 false”替代。

| 用例 | 选择值 / 能力与窗口结果 | 预期窗口 | 预期警告 / 诊断调用 |
|---|---|---|---|
| 默认模型未声明 | 空；报告 A；窗口 `(0,false)` | C | model=A；1 次 |
| 非正数声明 | 空；报告 A；分别 `(0,true)`、`(-1,true)` | C | model=A；1 次 |
| 已声明默认窗口 | 空；报告 A；`(4096,true)` | 4096 | 无；0 次 |
| 声明大于 C | 空；报告 A；`(16384,true)` | C | 无；0 次 |
| 显式模型 | B；报告 A；未声明 | C | model=B；0 次 |
| 报告空值 | 空；报告空；未声明 | C | model=""；1 次 |
| 无报告能力 | 空；窗口未声明 | C | model=""；无调用 |
| 只有报告能力 | 空；报告 A；无窗口接口 | C | model=A；1 次 |
| 两种能力均缺失 | 空；普通 Provider | C | model=""；无调用 |
| 禁止二次解析 | 空；仅查询参数为 A 时才返回正数；报告 A | C | model=A；窗口只收到一次空字符串 |
| nil Provider | nil | C | 无记录、无调用 |

凡有窗口接口，每轮断言它只收到一次原始选择值；有效声明和显式选择用例断言诊断方法没有被调用。另用一个实际 Run 的局部替身捕获 ChatStream options，确认默认归因 A 后请求仍收到空 Model。

### 7.2 去重与热切换

| 同一 Agent 中的调用序列 | 预期警告记录 |
|---|---|
| 同 provider，空选择值、默认 A，重复两轮 | 1 条，A |
| 同 provider，先默认 A，后显式 B，再回默认 A | 2 条，A 和 B |
| 同 provider，先默认 A，后显式 A | 1 条，A |
| 两个同名实例分别默认 A、B | 2 条，A 和 B |
| provider p 默认 A，再切到 provider q 显式 B | 2 条，provider 分别为 p、q |
| 缺失能力或空返回，空选择值重复 | 1 条，空名称 |
| 新建两个 Agent，各遇到同一组合 | 每个各 1 条 |

至少一个持久线程用例沿用 `provider_window_test.go` 的 Memory Store 和 SetThreadConfig 模式：首轮无模型、报告 A，第二轮显式 B；断言两条属性记录和两次请求的原始 options。跨 provider 切换同时传 Provider 与 Model，遵守现有配置契约。

并发测试让多个调用同时计算同一未声明组合，断言仅一条记录。替身计数使用同步保护，日志沿用 JSONHandler 加互斥 bytes.Buffer 的方式收集，或在现有 capture 外包同步 Handler；不并发使用未加锁的 truncationLogCapture。

### 7.3 适配器与包装器

| 位置 | 表中输入 | 断言 |
|---|---|---|
| 四适配器各一个 TestDefaultModelName | 构造模型 A、空字符串 | 满足新接口；分别返回 A、空字符串 |
| 四适配器同一测试表 | 构造模型带大小写和空格 | 原样返回，不归一 |
| llm 实际包装 | 底层报告 A；登记 `"":4096` | 包装不满足新接口；空键仍命中 |
| llm 实际包装 | 底层报告 A；仅登记 A | 空键未命中，不向底层解析 |
| llm 不包装 | nil、空表、全部非正数表 | 返回原对象，报告能力保留 |
| agent 加实际包装 | 底层报告 A；分别登记空键或仅 A | 前者无警告；后者 model="" |
| Guard 名称转发 | 底层报告 A；新鲜、恰好过期、早于验证时间 | 均返回 A；报告不查询窗口、不采样时钟 |
| Guard 空名称 | 底层返回空；底层缺失报告能力 | Guard 均满足新接口并返回空 |
| Guard 嵌套实际包装 | 底层先被 WithContextWindows 包装 | 不越层读取默认名，返回空 |
| Guard 过期进入 agent | 空选择值，底层报告 A | 窗口回退 C，警告 model=A |
| Codex 空模型运行 | endpoint 有效（通过 WithAuthSource 注入非 nil 的 auth.Source 替身）、短历史、无窗口登记、无显式模型 | 先出现空 model 警告，再得到 model_required；Token/Refresh 如被调用即使测试失败；不发网络请求 |

Guard 测试沿用同包 `modelmeta/refresh_test.go:35-39` 定义的 `setClock`。涉及全局时间替身的测试不并行；测试报告方法不读时钟时，可设置调用即失败的时钟替身。Codex 的运行用例放在 agent 包测试中，保持 endpoint 有效，通过 `WithAuthSource` 注入非 nil 的 `auth.Source` 替身，其 `Token` 和 `Refresh` 一旦被调用就使测试失败。endpoint 检查先于空模型检查；实际刷新发生在 `llm/codex/transport.go:18-19` 的 openStream 中，因此该用例无需真实凭据或网络，仍验证先警告、后 model_required 的顺序。

### 7.4 实施检查

实现完成后执行仓库已有检查：

```bash
go build ./... && go vet ./...
go test -count=1 ./...
go test -race -shuffle=on -count=1 ./agent ./store ./modelmeta
gofmt -l .
bash scripts/kernel-closure-guard.sh
bash scripts/check-readme-contract.sh
```

验收要求为构建、vet、测试和脚本成功，gofmt 不输出文件。已有不相关失败需单列说明；本改动导致的失败必须修复。本任务只有设计文档，不能把这些命令写成已经通过的结果。

## 8. 决策点汇总

接口形式、诊断范围和缺失退化已经确定，不再作为选项。下面只保留一个需要 owner 确认的公开 API 选择；其余为本设计的具体实施建议，理由分别见第 3 至第 5 节。

| 编号 | 决策 | 推荐 | 理由 |
|---|---|---|---|
| D1 | 新能力的公开命名与空值约定 | `llm.DefaultModelReporter`，方法 `DefaultModelName() string`，空值表示无法报告 | Reporter 限定信息报告用途；本任务不需要区分“无默认模型”和“名称未知”，无需第二返回值 |

Guard 转发、内核查询位置、WithContextWindows 的说明、agenttest 保持现状、日志断言方式均已有明确建议，不需要拆成额外产品裁决。

## 9. 风险与残留问题

- 第三方 Provider 或包装器不实现、未转发新接口时，警告仍可能为空。这是约定的退化；内核不会越过包装层窥探底层。
- Guard 始终增加新方法，类型断言会从失败变为成功，即使底层没有名称。空返回明确表示不可用，避免调用方把“满足接口”当成“必有名称”。
- 同名 provider、相同最终名称继续共用一个去重项；空能力也继续共用原来的空名称项。此设计不区分实例、默认与显式来源，不改变既有 provider 命名职责。
- 第三方实现返回变化中的名称可能增加去重项，返回错误名称也会错误归因。内核只使用其报告值；仓内适配器直接读取构造字段，不引入额外可变状态。
- 公开方法与默认字段未来可能脱节，适配器请求默认值变化时须同步检查报告方法。每包测试直接比较构造输入和报告值，现有请求转换测试继续覆盖请求本身。
- 本次已从源码确认流程，尚未实现或运行上述新增测试。没有需要补充外部条件才能完成的设计项；公开 API 名称待 owner 确认。
