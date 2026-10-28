# tool/Registry Info 冻结与读缓存设计（D2a + D3）

> 状态：设计稿（2026-10-28，待 owner 裁决 §11 开放决策点；裁决后 docs/DESIGN.md 修订草案先行落地，再进代码）
> 前置：外部调研（MCP go-sdk / genkit / OpenAI Agents / LangChain / Eino）已由 owner 裁决 2a——注册时预计算 ToolInfo、`List` 返回缓存副本、`Info()` 按注册时刻不可变处理；D3（依托 2a 增加序列化缓存，两层结构已草拟）。本文档细化两层结构、写/读路径、失败语义、并发不变量与测试计划，并给出 DESIGN.md 修订草案原文。只产出设计，不含实现。

## 1. 问题：`List` 在 RLock 内执行用户代码

`Register`/`Replace`/`Apply` 的校验（含 `t.Info()` 调用）刻意放在锁外（registry.go:23 `validateForRegistration` 注释明示"user-supplied Info() is never called while holding it"）；但 `List`（registry.go:59）在 `RLock` 内对每个工具调 `t.Info()`。三个具象危害：

1. **确定性死锁三角。** 设工具 X 的 `Info()` 内部调 `r.Register(z)`（封装器聚合、动态描述都可能走到这形状）：
   - goroutine A 调 `List()` → 拿到 RLock → 调 `X.Info()` → `Info()` 调 `r.Register(z)` → `Register` 走完锁外校验后阻塞在 `mu.Lock()`（排队等写锁）；
   - A 要释放 RLock 必须等 `Info()` 返回，`Info()` 要返回必须等 `Register` 完成，`Register` 要完成必须等 A 释放 RLock——三角互等，永久死锁。
   - 变体：`Info()` 重入 `List()` 时，若恰有写者排队，Go RWMutex 把新来的 RLock 挡在写者后面，同样互等。无写者排队时"碰巧"能跑——生产偶发、压测必现的典型形态。
2. **慢 `Info()` 阻塞一切写者。** `Info()` 是任意用户代码（大 schema 构造、远程查询皆可能）；它在 RLock 内执行的整段时间，所有 `Register`/`Replace`/`Apply`/`Unregister` 排队等待。
3. **漂移别名。** `Info()` 返回值若引用工具内部状态：工具事后修改 schema，已交付的 List 快照跟着漂移；`Info()` 每次返回不同 Name 时，List 显示名与 map 键脱节——List 报 X，`Get("X")` 却 miss。

调研的全部对照运行时（MCP go-sdk、genkit、OpenAI Agents、LangChain、Eino）都不在共享锁内执行用户描述回调——owner 已据此裁决 2a。

## 2. 已裁决前提与本文档的分工

- **继承（不再讨论）**：2a——`Info()` 注册时刻预计算、`List` 返回缓存副本、"Info 不可变于注册时刻"为采纳语义；D3——依托 2a 增加序列化缓存，两层结构框架。
- **本文档细化**：条目/快照的精确数据结构、写路径各方法的行为、marshal 在锁内还是锁外、`List` 返回值的拷贝纪律、agent 交互确认、失败语义、不变量枚举、测试计划、DESIGN.md 修订原文。
- **本文档新提出的取舍**（列为 §11 开放决策点，各带推荐）：序列化字节的去留、`List` 深拷贝 vs 浅共享、marshal 入口选择、惰性重建机制。

## 3. 数据结构变更（全部未导出；公开 API 零变更）

设计规格（非实现代码）：

```go
// registryEntry 是 map 的值类型。插入后三个字段全部不可变（不变量 I4）。
type registryEntry struct {
    tool       Tool     // 执行 handle；Get 原样返回，语义不变
    info       ToolInfo // 冻结 canonical：写路径锁外深拷贝而来；嵌套状态永不逃逸出 tool 包
    paramsJSON []byte   // json.Marshal(info.Parameters) 的缓存；确定、插入后不变（I8）
}

type Registry struct {
    mu       sync.RWMutex
    tools    map[string]registryEntry // 键 = 冻结 info.Name（唯一，I3）
    snapshot []ToolInfo               // nil = 脏，需重建；非 nil = 与当前 tools 严格一致的按名升序浅拷贝数组
}
```

- 变更点一：`tools` 的值类型从 `Tool` 改为未导出的 `registryEntry`（键、语义不变；包外不可见）。
- 变更点二：新增未导出字段 `snapshot`。
- 新增未导出辅助：`freezeForRegistration`、`deepCopyToolInfo`（递归含 `ParameterSchema`/`Property` 的嵌套 map/slice/指针，逐字段保真 presence 标志）、`rebuildSnapshotLocked`。

**公开 API 零变更**，逐项确认：

| 公开标识符 | 变更 |
|---|---|
| `Tool`、`ToolInfo`、`ParameterSchema`、`Property`、`SchemaKeyword`、`ToolResult` 及构造函数 | 无 |
| `NewRegistry` / `Register` / `MustRegister` / `Get` / `List` / `Unregister` / `Replace` / `Apply` | 签名与既有语义全部不变 |
| `List() []ToolInfo` | 签名不动；仍按名升序；空注册表返回**空非 nil 切片**（与今日一致） |
| `Get(name) (Tool, bool)` | 不动；仍返回活的执行 handle |

零新增公开标识符、零签名变更、零行为兼容性影响（对 §6 定义的契约合规工具）。

## 4. 写路径

所有写入共享同一条流水线（单一构造点，对应 D3 草案"write path computes info+marshal inline"）：

```go
func freezeForRegistration(t Tool) (registryEntry, error) {
    info, err := validateForRegistration(t) // ① 用户 Info()：锁外，全库唯一调用点（语义与今日完全一致）
    if err != nil {
        return registryEntry{}, err
    }
    frozen := deepCopyToolInfo(info) // ② 冻结深拷贝：纯函数、零用户代码、锁外
    params, err := json.Marshal(frozen.Parameters) // ③ 序列化：锁外（理由见下）
    if err != nil {
        return registryEntry{}, fmt.Errorf("tool %q: serialize parameters: %w", frozen.Name, err)
    }
    return registryEntry{tool: t, info: frozen, paramsJSON: params}, nil
}
```

`deepCopyToolInfo` 的拷贝规格：切片一律克隆底层数组——`SchemaKeyword.Value`（`json.RawMessage`）与 `Required`/`Enum`（`[]string`）都不与输入共享可变 backing array；`*bool`（`AdditionalProperties`、`BooleanSchema`）总是新分配。

**marshal 放锁外的裁决与理由**：①之后输入 `frozen` 是本地值，不依赖任何注册表状态，锁没有正确性必要；marshal 内部跑 codec 的 `validateSchemaGraph`（reflect 建图、非平凡），放锁内会无谓拉长写临界区、阻塞读者；与"`Info()` 及一切可在锁外完成的工作都在锁外"的既有原则对称。唯一反方论点（锁内可让 entry 构造与插入原子）不成立：entry 在插入前就已完整，不存在需要锁保护的中间态。**深拷贝同样在锁外**（②在③前：marshal 冻结值，保证缓存字节与 `List` 交付值同源，见 §5）。

各方法行为（校验时序、错误文本、零变更保证均与今日一致，只有"插入什么"变化）：

| 方法 | 行为 |
|---|---|
| `Register` | `freezeForRegistration`（锁外）→ Lock → 重名检查（不变）→ `tools[name] = entry` → **同一临界区内 `snapshot = nil`** → Unlock |
| `Replace` | 同上，仅存在性检查方向相反（名字不存在报错，不变）；换入**全新 entry**（新 canonical），从不就地改写旧条目 |
| `Apply` | 对每个 added 逐一 `freezeForRegistration`（全部锁外；任一失败零变更）→ Lock → 既有 remove/dup 检查（不变）→ 统一批量写入 → **一次** `snapshot = nil` → Unlock。批量折叠为一次重建 |
| `Unregister` | Lock → `delete` → **仅当删除实际发生时**置 `snapshot = nil` → Unlock；名字本不存在则零变更、不置脏，与 Register/Replace/Apply 的零变更失败路径对称。不碰 Info、不 marshal（对应草案"Unregister just deletes"） |
| `MustRegister` | 透传 `Register`，不变 |

`validateForRegistration` 的签名、语义、错误文本保持原样（含其"锁外调用 Info"的注释契约），`freezeForRegistration` 是其后的新增步骤——最小扰动既有白盒测试。

## 5. 读路径

### List

```go
func (r *Registry) List() []ToolInfo {
    r.mu.RLock()
    snap := r.snapshot
    r.mu.RUnlock()
    if snap == nil {                       // 惰性重建（Layer 2）：单锁 double-check
        r.mu.Lock()
        if r.snapshot == nil {
            r.rebuildSnapshotLocked()      // 排序 + 浅装配（N 个结构体拷贝 + sort），零 codec 调用
        }
        snap = r.snapshot
        r.mu.Unlock()
    }
    out := make([]ToolInfo, len(snap))
    for i := range snap {
        out[i] = deepCopyToolInfo(snap[i]) // 深拷贝在锁外完成（安全性由 I4/I5 支撑）
    }
    return out
}
```

- **快照命中路径**（稳态）：短暂 RLock 取数组头 → 锁外深拷贝。零用户代码、零 codec、零排序。
- **未命中路径**（变更后的首次 List）：写锁内重建一次。置脏与 map 变更在同一写临界区、重建在写临界区内完成 ⇒ 不存在 lost invalidation（不变量 I6）。
- 排序、空切片语义与今日一致；名字唯一（I3）⇒ 全序确定。

**拷贝纪律裁决：`List` 逐调用深拷贝。** 理由：

1. **别名风险归零。** 浅共享方案下，任一消费者对返回值嵌套状态的误写（改 `Properties` 条目、就地改 `Required[0]`、改 `Keywords[i].Value` 字节）会永久污染注册表 canonical 与同代所有 `List` 交付物——而 DESIGN.md P2-4 明言"Registry 是进程级共享状态"，爆炸半径是共享它的全部 Agent。深拷贝把误写半径限制在消费者自己的副本里。
2. **仓库既有纪律一致。** agent 的 `deepCopyMessages`（"history deep-copied, never mutate caller slices"）、`agenttest` 的 Request 深拷贝与 `ToolFunc.Calls` 受锁深拷贝快照，同为"读边界交付深拷贝"的先例。
3. **成本可忽略（量化）。** 典型 schema 10–50 节点，全树深拷贝约 1–5 µs/工具；40 工具的 `List` 在几十 µs 量级，至多每 iteration 一次（`dynamicTools`）；同 iteration 的模型 HTTP 往返 ≥ 数百 ms——差五个数量级。旧 `List` 在锁内跑的 N 次 `Info()` 是无界用户代码，新路径成本与之同数量级甚至更低。
4. 备选（浅共享冻结快照）省下的只是 µs 级拷贝，换来的是"全进程共享缓存可被第三方静默写坏"的故障模式，与设计目标 1（稳定）冲突。

注意分层：**写路径的冻结深拷贝（§4②）与读路径的交付深拷贝（本节）各自必要**——前者把 canonical 与工具内部状态隔离（落实 2a 的"注册时刻冻结"，否则缓存会跟着工具漂移，等于没冻）；后者把消费者与 canonical 隔离。

### Get

不变：RLock 下返回 `entry.tool`（活 handle）与存在性。执行路径需要的正是活的 `Tool`，不经缓存。

### 快照数组本身

`rebuildSnapshotLocked` 产出 cap==len 的数组，元素为 canonical 的浅结构体拷贝（共享 canonical 嵌套状态，但该数组与 canonical 嵌套状态都只在包内可达）；数组建成后整体替换、从不就地改写（I5），故 `List` 锁外读它安全。

## 6. 与 agent 的交互（确认：除 Info 冻结外零可观察变化）

`Registry.List()` 生产调用点共三处：`runStreamInternal` 内的 agent.go:371（run 起点广播快照）与 agent.go:394（`WithDynamicTools()` 时的每轮迭代顶部刷新，位于 ctx 检查与 compaction 之间），以及 mcp/bridge.go:129——`Connect` 发现后 `prepareTools` 做撞名检查，只读 `info.Name`，缓存路径下名字与今日一致，零影响。测试侧另有 harness crash_test 与 e2e 的只读使用。

- **调用点、频率、快照语义零变化**：默认 run 级广播快照、dynamicTools 轮级刷新、同轮各 provider attempt 复用同一 `tools` 切片——全部照旧。冻结只改变"快照内容的来源"：从"取快照时刻的活 `Info()`"变为"注册时刻的冻结 `Info()`"。
- **对契约合规工具（`Info()` 稳定），两者恒等**：`List` 返回值逐字段相同，适配器产出的请求字节逐一相同，行为字节级不变。
- **对漂移工具（`Info()` 不稳定），行为按设计改变**：这正是 2a 采纳的语义（业界共识）。此类工具的合法刷新路径是 `Replace`——整体换新实现，原子重算 info + 序列化缓存。今日已存在的两类漂移 bug（快照跟着工具内部状态漂移、List 显示名与 Get 键脱节）被此次变更根治。
- **注册表旁路的活 `Info()` 调用点，有意保留、零改动**：
  - 并行规划边界 `toolInfoAtPlannerBoundary`（tool_execution_planner.go:75）：live `Info()` + panic recover 为 P2-2 冻结契约，不在任何注册表锁下；
  - 串行审批路径（agent.go:761/:769）：live `Info()`，不在锁下，panic 边界行为不变；
  - harness 审批包装器（harness/internal/app/approval_tools.go:42）：`runTool.Execute` 内对 Tool handle 直接调 live `Info()`（只取 Name），不涉及注册表锁，零影响。
  在冻结语义下活 `Info()` ≡ 注册表冻结值；改走注册表缓存需要新的公开查询 API（`Get` 只回 `Tool` 不回 info），超出本轮范围。
- **agent/ 目录零代码改动**；既有 dynamicTools/热变更测试继续守护轮级刷新契约。

## 7. 失败语义：注册期 marshal 失败

**新失败面只有一个**：`freezeForRegistration` 的 ③ marshal 报错 → `Register`/`Replace`/`Apply` 返回 `fmt.Errorf("tool %q: serialize parameters: %w", ...)`，**零变更**（③在锁外、在任何 map 写之前）。`MustRegister` 对该错误的反应与既有校验失败同类（panic），不变。

**该分支正常不可达**，证明：

1. `MarshalJSON` = `validateSchemaGraph` + `marshalParameter`；而注册校验 `validateRegistrySchema` = `validateSchemaGraph` + `validateTypedProperties`，是 MarshalJSON 前置条件的严格超集。
2. `marshalParameter` 的全部失败点逐一枚举：键名/描述/`$ref`/enum/required 的 `json.Marshal(string|[]string|bool)` 不可失败；组合子项为 nil 已被 `validateSchemaGraph` 的 "composition contains nil schema" 排除；carrier `Keywords` 的 Value 已被 `validJSONValue` 校验为单一合法 JSON 值；`jsonField` 对这些标量类型的封装同样不可失败。

⇒ 校验通过 ⇒ 注册时刻的值必然 marshal 成功。**无观察差异确认（限契约合规工具）**：注册后 `Info()` 不变的工具，"注册成功但请求期 `convertToolDefs` marshal 失败"的组合是空集（同一证明），marshal 前置到注册期对其零可观察差异；今天会被拒的注册依然被拒、错误文本不变（marshal 分支不触发）。**漂移工具是例外，且差异是改善**：今天 `List` 交付活 `Info()`，适配器在请求期 marshal 漂移后的值（openai adapter.go:431、glm adapter.go:529），漂移成不可 marshal 的状态会让整次 run 中途失败；冻结后 `List` 只交付注册时刻已验证、已序列化的副本，该请求期失败路径被一并消除——正是 2a 语义要的行为。该分支的存在价值是防御性前置——若未来 codec 增加可失败路径，错误在注册期带工具名显式暴露，而不是 run 中途在适配器处炸掉整次运行。深拷贝（②）为纯内存操作，无失败面。

## 8. 并发不变量（枚举）

- **I1 锁守卫范围**：`mu` 是 `tools` map 与 `snapshot` 字段的唯一守卫；两者的一切读写都在锁内。`List` 锁外的深拷贝只读取不变对象（由 I4/I5 保证安全），不构成例外。
- **I2 用户代码边界**：用户代码（`t.Info()`）只在 `validateForRegistration` 内执行，永远先于任何锁获取。Registry 全部方法在持锁期间零用户代码、零 schema 序列化、零 codec 校验、零 I/O。P2-4 的"从不回调用户代码"从变更方法扩展为**全部方法无条件成立**。
- **I3 键唯一与全序**：map 键 = 冻结 `info.Name`，`Register`/`Apply` 查重保证唯一 ⇒ 排序结果确定。
- **I4 条目不可变**：`registryEntry` 三字段插入后永不再写；`Replace` 换入全新条目，从不就地修改。写路径锁外深拷贝保证 canonical 嵌套状态与工具内部状态无别名——这是"注册时刻冻结"的执行机制。
- **I5 快照数组不可变**：建成后整体替换（置 nil / 重建整体赋值），从不就地改写元素；cap==len，调用方 append 必然重分配。
- **I6 快照一致性（无 lost invalidation）**：非 nil `snapshot` 与当前 `tools` 严格一致。置脏与 map 变更在同一写临界区、重建在写临界区内完成，互斥性排除"重建后又被更早的置脏覆盖"的交错。
- **I7 深拷贝纯函数**：`deepCopyToolInfo` 零用户代码、不改输入、逐字段保真 presence 标志（`typeAbsent`/`propertiesInactive`/`refPresent` 等，字节级 round-trip 由测试锁定）。
- **I8 缓存字节确定**：`paramsJSON` = `json.Marshal(冻结 Parameters)`；codec 对 properties/keywords 按名排序 ⇒ 同值同字节；插入后不变。
- **I9 运行时形态不变**：不新增 goroutine、不等待在飞 Execute、不新增锁种类（沿用单一 `sync.RWMutex`）。
- **I10 空注册表快照非 nil**：干净快照对空注册表必须是**非 nil 空切片**。`nil` 专用于脏标记，不得与"空"混叠——否则空注册表的每次 `List` 都误判为脏而重建。

## 9. 测试计划（stdlib-only、table-driven、包内白盒）

新增（与既有 `registry_test.go` / `registry_mutation_test.go` 并列，风格一致）：

1. **`TestListNoUserCodeUnderLock`（死锁回归，确定性设计）**：变体 = `Info()` 内分别调用 `Register` / `Replace` / `Unregister` / `List`。其中 `Info()` → `r.Register(z)` 变体在**旧代码下是确定性死锁**（A 持 RLock 等 Info 返回，Info 内的 Register 等写锁，写锁等 A 释放 RLock——三角互等）；新代码 `List` 根本不调 `Info()`，瞬时完成。结构：`done` channel + `select { case <-done: case <-time.After(5s): t.Fatal("registry deadlock: List ran user Info() under lock") }`——通过不依赖时序，看门狗只兜底。
2. **`TestListInfoCallCount`**：计数 `Info()` 的工具（atomic 计数器）；Register 后多次 `List`、并发变更后再 `List`，计数恒为 1（只在注册时调用过）。
3. **`TestListFrozenAtRegistration`（冻结语义 enforcement，table-driven）**：变体 = 注册后改工具内部 schema / 改 Description / `Info()` 每次返回不同 Name（计数器）/ 改 Properties 内容。断言：`List` 返回注册时刻值；`Get` 按注册名命中；序列化字节不变。
4. **`TestListSnapshotLifecycle`（白盒）**：各变更后 `r.snapshot == nil`；`List` 后非 nil；无变更的第二次 `List` 不重建（以 `rebuildSnapshotLocked` 内递增的包级测试缝 `snapshotRebuilds` 断言，仓库惯例的 package-var seam）；`Apply` 批量只重建一次。空注册表变体：首次 `List` 后 `snapshot` 为非 nil 空切片（I10），第二次 `List` 不重建（`snapshotRebuilds` 计数不变）。
5. **`TestListDeepCopyIsolation`（别名隔离，扩展既有 `TestRegistryListReturnsCopy`）**：对返回值做全嵌套破坏性修改——`Properties` 增/删/改条目、`Required` append 与就地改、`Keywords[i].Value` 字节改、`Items`/组合/AP-schema 深处改、解引用改 `AdditionalProperties`——再 `List` 应与原始冻结值完全一致（`reflect.DeepEqual` + marshal 字节双重断言）；两次 `List` 的返回值互不影响。既有断言（改 `list[0].Name` 不影响注册表）保留并继续成立。
6. **`TestFreezeFidelityBytes`（深拷贝保真，table-driven schema 矩阵）**：覆盖 nullable、items 嵌套、anyOf/oneOf/allOf、`$ref` presence、boolean schema、carrier keywords、AP bool / AP schema、explicit-empty description/type（唯一产生 pathological presence 的外部构造路径）。断言 `json.Marshal(deepCopy(x))` 与 `json.Marshal(x)` **逐字节相等**，且等于 `entry.paramsJSON`。
7. **`TestRegistryConcurrentStressCache`**：扩展现有 `TestRegistryConcurrentMutations`——List 循环 × Register/Unregister/Replace/Apply 循环并发，终态一致性断言沿用现有模式；`go test -race` 全绿。
8. **marshal 失败分支**：按 §7 证明不可经 exported 表面构造，**不伪造不可达输入、不放松校验制造失败**；以等价性测试替代（6 的 `entry.paramsJSON` 一致性 + "所有注册成功工具再 marshal 必成功"）。

既有测试全部不改预期通过：`List_SortedOrder`、`List_Empty`（空非 nil 切片）、`ConcurrentAccess`、`Unregister`/`Replace`/`Apply` 系列、`ListReturnsCopy`。agent 侧零改动 ⇒ 无新增 agent 测试的硬要求；实施时全量 `go test -count=1 ./...`，并建议把 `./tool` 加入既有 `go test -race -shuffle=on -count=1 ./agent ./store` battery（并发语义是本次核心变更）。

## 10. docs/DESIGN.md 修订草案（原文）

插入位置：`### P2 扩展面` 内、P2-4 条目列表之后、`### P3 生态` 之前。另在 §3 契约表 `tool.Tool` 行末追加一句（第二处编辑，可选但建议）。

```markdown
#### Registry Info 冻结与读缓存（P2-5，已裁决 2026-10-28：2a + D3）

- `Info()` 按注册时刻不可变处理（业界共识语义，经外部调研裁决）：`Register`/`Replace`/`Apply`
  在取锁前调用一次 `Info()` 完成全部校验，随后深拷贝冻结为注册表内部 canonical，并把冻结
  schema 的序列化字节缓存于条目内。注册后工具实现修改自身状态、或 `Info()` 返回漂移值
  （含改名），均不影响已注册视图；需要刷新元数据时用 `Replace` 整体换实现（原子重算缓存）。
- `List()` 不再调用任何用户代码：返回排序快照的逐调用深拷贝（嵌套 map/slice/指针不与注册表
  共享），排序、空切片语义不变；`Get` 不变。Registry 任何方法在持锁期间不执行用户代码、
  不执行 schema 序列化；慢 `Info()` 不再阻塞写者，`Info()` 重入 `List` 或写方法不再死锁。
- 两层缓存：条目层（tool + 冻结 info + 序列化字节，插入后不可变）；聚合层（排序快照，写路径
  在同一临界区置脏，`List` 惰性重建，批量 `Apply` 折叠为一次重建）。重建只做排序与装配，
  深拷贝与 marshal 都在锁外。条目内序列化字节与 `List` 交付值的序列化逐字节一致；本轮无包外
  消费者，为后续适配器请求装配快路径预置。
- 注册时序列化失败即注册失败、零变更：现有校验严格覆盖序列化前置条件，该分支正常不可达，
  属防御性前置——把理论上可能的请求期失败移到注册期带工具名显式失败。
```

§3 契约表 `tool.Tool` 行追加（行末接排）：

```markdown
`Info()` 按注册时刻冻结入注册表（见 P2-5），`List` 只返回冻结副本，不再回调用户代码。
```

## 11. 开放决策点（各带推荐；裁决后按 §12 落地）

**D-1 序列化字节存不存。** 机制：条目层缓存 `paramsJSON`（每工具一份 schema JSON 字节，数十至数百字节）。现状：在"公开 API 零变更"约束下，本轮**包外没有任何消费者**——适配器仍按次 `json.Marshal(t.Parameters)`（每次请求、每次重试 attempt 各一遍），缓存字节要产生收益需要未来的 Provider 侧快路径（例如请求装配时复用预序列化参数，属 Provider/ToolInfo 运载扩展，不在本轮）。marshal 本身仍会在写路径执行（失败语义前置 + 请求期不可达证明不依赖是否存字节），所以真正的取舍只是"存不存这份暂时无人读的字节"。选项：(a) 按 D3 原样保留存储【推荐】——D3 已裁决，成本仅每工具一份字节，未来快路径落地即接通；(b) 只验不存（YAGNI）——省一份内存，快路径设计时再加回，需再动一次结构。推荐 (a)；若 owner 严守 YAGNI 可改 (b)，对设计其余部分零影响。

**D-2 `List` 拷贝纪律。** 机制：见 §5。深拷贝每调用多付 µs 级成本，换来消费者误写无法污染进程级共享缓存；浅共享省该成本，但任一消费者的嵌套误写会静默腐蚀共享它的全部 Agent 的工具视图（今天这类误写只会打到工具自身状态）。推荐：深拷贝（本设计采用）。若未来出现"工具数巨大 + `List` 极高频"的真实画像，可凭数据再评。

**D-3 写路径 marshal 入口。** 机制：`json.Marshal(frozen.Parameters)`（公共 canonical 路径，内部会再跑一遍 `validateSchemaGraph`，写路径图校验约翻倍——注册是低频操作，无感）；或包内直调 `marshalParameter`（跳过二次校验，但缓存字节不再与适配器走的公共路径同源，等价性从"同一代码路径"退化为"靠测试锁定"）。推荐：`json.Marshal`，同源即逐字节一致的构造性保证。

**D-4 惰性重建机制。** 机制：单 RWMutex double-check（未命中 `List` 短暂升级写锁重建，稳态读纯 RLock）——实现直白、无 lost-invalidation 论证负担（I6）；或 `atomic.Pointer` 快照（重建可全程在 RLock 内完成、永不升级写锁，但"Store 必须在 RUnlock 前完成"的正确性论证较细，需专门 stress 测试）。推荐：单锁 double-check——重建只在变更后的首次 `List` 发生（低频），升级写锁的代价可忽略，换来与包内既有同步风格一致。

## 12. 落地清单（裁决后执行；本轮零代码）

- 生产代码：仅 `tool/registry.go`（结构 + 三个未导出辅助 + 各方法接线）；`agent/`、`llm/`、`agenttest`、`mcp`、`harness`、`examples` 零改动。
- 测试：新增 §9 各项（并入既有两测试文件或新文件，随包内惯例）。
- 文档：docs/DESIGN.md 按 §10 落地（契约先行）；**§10 草案须按 D-1～D-4 的最终裁决重新渲染后再落地**——例如 D-1 若裁"只验不存"，第三条中"序列化字节缓存于条目内"等表述需同步删改；tool/AGENTS.md 锚点随 registry.go 结构变更重生成。
- CI：建议 `-race -shuffle=on` battery 加入 `./tool`。
- 依赖闭包：仅用 `sync`/`sort`/`encoding/json`/`fmt`，kernel-closure-guard 不变；无 `pkg/`；错误一律 `fmt.Errorf` 小写上下文 + `%w`；不触碰 sealed 接口。
