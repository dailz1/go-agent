# 30 · 持久化后端（store 包）

> 内核文档集第 30 篇。本文只讲 `store` 包：`Store` 接口契约与 `Record` 记录形态、JSONL 后端的文件布局与崩溃一致性、内存后端、checkpoint 在字节层面的格式与「可丢弃重建」的存储侧根据，以及新增一个后端需要满足的隐含契约（从测试断言反推）。
> 分工边界：第 12 篇（12-agent-persistence.md）拥有**协议**——记录 kinds 的语义、declare/commit 顺序、重放状态机、settle/恢复语义；本文拥有**字节**——追加、校验、排序、崩溃修复、加速读取。第 14 篇（14-agent-control-plane.md）拥有控制面记录（`config_changed` / `input_queued` / `input_consumed`）的写入侧语义；第 00 篇 §4.4（00-overview.md）给出这个接缝的定位。持久化的总裁决在 docs/DESIGN.md「P1 运行时安全」（docs/DESIGN.md:89-144），本文与其冲突时以 DESIGN.md 为准。
> 行号锚点基于 2026-10-29 工作区（基线 65fea54，含 provider 热换未提交改动），全部经 grep 校验；文中标注「实测」的行为均以本机复现为准。

## 1. `Store` 契约：四个方法与一条记录

### 1.1 接口为什么这么小

```go
type Store interface {                                          // store/store.go:113
    Append(ctx, thread, expected, records...) (int64, error)    // :127 乐观并发写 + 幂等重试
    Latest(ctx, thread) (ThreadState, error)                    // :131 最新 checkpoint + 增量尾
    History(ctx, thread, from) ([]Record, error)                // :140 全量回退读
    Delete(ctx, thread) error                                   // :146 整线程删除
}
```

四个方法，每个只守一件事：写（Append）、加速读（Latest）、全量读（History）、删除（Delete）。这个面刻意收窄到「一个只会追加和读取的东西也能实现」的程度，背后有三条删减决策：

**没有显式 create——线程由第一次 Append 隐式创建。** 接口注释写明「Threads are created implicitly by the first Append (expected = 0); there is no explicit create」（store/store.go:111-112）。这不是省事，而是把「存在性检查 + 创建」压成一个原子动作：以 expected=0 追加，要么线程不存在且创建成功，要么线程已存在且得到 `ErrRevisionConflict`——一步完成，没有读-写两步之间的 TOCTOU 窗口。agent 层生成线程的 rev=0 预留协议（第 12 篇 §4.3）完全建立在这个原子性上：若存在独立的 `CreateThread`，预留就要「先 create 再 append」，两步之间的崩溃会产生空壳线程。同理，head=0 即「新建」、没有「线程不存在」错误（docs/DESIGN.md:102），让「续接或创建」对调用方是一个动作。

**没有枚举——线程 ID 与发现权归调用方。** 包注释：「Thread IDs and their discovery/indexing are caller-owned: the Store never enumerates threads」（store/store.go:14-15）。内核不提供线程列表 API，因此 Store 契约也不需要 scan/list；一个数据库后端可以在内部建索引，但契约不要求它暴露。这条删减同时保住了并发契约的最小性：因为不存在「发现别人的线程」，单进程、单写者的假设（store/store.go:11-13）就够用。

**没有 Close——生命周期管理是具体后端的事。** `Close` 只存在于 `JSONLStore`（store/jsonl.go:106）这个具体类型上，内存后端根本没有。agent 从不关闭 store：构造与拆卸归宿主，接口只建模「能做什么」，不建模「怎么回收」。

更宏观的一条：**「agent 拥有协议，store 拥有字节」**（00-overview.md §4.4 的表述）。若 store 也理解生命周期语义（`round_declared` 必须配对 `round_committed` 之类），每个后端都得跟着 agent 协议版本走，协议每演进一次（R07 取消结算、控制面、codec 3）就要改一遍所有后端。现状是协议的全部裁决都住在 agent 的 persist*.go，store 只认字节与顺序——所以 SchemaV2 一揽子新增时 DESIGN.md 可以写「不改变 Store 接口」（docs/DESIGN.md:116），后端零改动。

### 1.2 `Record`：调用方字段与 store 字段的分界

```go
type Record struct {          // store/store.go:83
    Seq        int64           `json:"seq"`         // store 指派：位置/修订号
    Kind       string          `json:"kind"`        // 调用方内容
    Schema     int             `json:"schema"`      // 调用方内容（缺省归一为 1）
    ID         string          `json:"id"`          // 调用方内容：身份
    RecordedAt time.Time       `json:"recorded_at"` // store 指派：观测用
    Payload    json.RawMessage `json:"payload,omitempty"` // 调用方内容：不透明字节
}
```

注释钉死了分界：「Seq and RecordedAt are assigned by the Store on Append; ID, Kind, Schema, and Payload are the caller's immutable content: an ID permanently identifies exactly one record, and a retry must reproduce identical content」（store/store.go:79-82）。两组字段各自回答一个不同的问题：

**`ID` 是身份，为什么由调用方铸造而不是 store 生成。** 幂等重试的前提是「重试逐字节复现同一内容」——若 store 在写入时铸造 ID，一次「已落库但 ack 丢失」的模糊失败之后，重试会铸出第二个 ID，日志里出现两条本该是同一条的记录。调用方铸造 ID（agent 侧用 `recordID(runID, suffix)` 等确定性派生，第 12 篇 §5.5）使重试内容可复现，`planAppend` 才能靠 ID + 全内容比对把重试分类成「已应用前缀」。整套 settle/Resume 的重试语义表（第 12 篇 §2.5 的 E14/E15/E16）都踩在这块地基上。

**`Seq` 是位置，为什么有了 ID 还需要它。** ID 是不透明字符串，没有顺序语义；而乐观并发要一个修订号（`Append` 的 expected 就是「我看到的 head」）、checkpoint 要一个定位锚（快照定位在自身 Seq）、`History(from)` 要一个地址。Seq 由 store 在写入时按位置顺序指派（`stamped[i].Seq = head - int64(len(apply)) + int64(i)`，store/jsonl_ops.go:50），因此天然稠密、从 0 起、值即下标——JSONL 后端甚至把「Seq 必须等于行号」作为加载时的完整性检查（store/jsonl_file.go:59-61）。

**`Payload` 是不透明字节。** store 从不解释 payload 的语义（那是 agent 的 DTO，第 12 篇 §5），它在 store 里的全部待遇是：写入前 `json.Valid` 校验（store/store.go:237-239）、深拷贝隔离（store/store.go:168）、幂等比对时 `bytes.Equal` 逐字节比较（store/store.go:193）。**幂等是字节级的，不是语义级的**：两次「语义相同但序列化不同」的重试会冲突。这就是 agent 侧那么多「纯函数构造记录、重试不重算」纪律（`cancelRecord` 纯函数、checkpoint 冲突后从活投影重新序列化）的存储侧原因。

### 1.3 kinds 与 schema：写侧从严，读侧放行

store 认识 11 种 kind（store/store.go:41-53），但对它们的「理解」仅限名字与版本规则，语义一概不知：

| kind | schema 约束 | 语义归属 |
|------|-------------|----------|
| `run_started` / `round_declared` / `round_committed` / `agent_event` / `error` / `checkpoint` | 1/2/3 皆可（checkpoint 按 codec 时代） | agent 生命周期协议（第 12 篇 §1.1 表） |
| `run_cancelled` / `config_changed` / `input_queued` / `input_consumed` / `run_closed` | **必须恰好 = 2**（store/store.go:227-229） | agent 拥有、但由控制面外部写者写入（第 14 篇） |

校验策略是刻意的**不对称**：

- **写侧从严。** `Append` 拒绝未知 kind（`ErrUnknownKind`，store/store.go:220-222）——「a stale binary cannot write records it does not understand」：旧二进制不许写出自己不认识的东西。五种控制面/取消 kind 的 schema 被 store 侧钉死为 2：这些 payload 的 codec 冻结在 v2，出现别的版本意味着手改日志或写者有 bug，在落库前就拦下。
- **读侧放行。** 读取容忍未知 kind、原样透传（store/store.go:37-39 注释：「rejecting them is a codec policy (the agent layer decides which kinds affect state)」）。协议策略只住在 agent 重放器一处；如果 store 也对 kind 有意见，两层就可能对同一条记录做出不一致的裁决。agent 侧对未知 kind 的真实裁决是 `ErrIncompatibleLog`（第 12 篇 §5.3），那才是协议层的哨兵。

schema 版本规则（`validateRecord`，store/store.go:216-241）：合法区间 [SchemaV1, SchemaV3]（:225-226），越界（含负数）即 `ErrUnsupportedSchema`；unset（0）在 `prepareBatch` 里归一为 SchemaV1（store/store.go:158-159）——所以「忘了设 schema 的重试」仍与首次写入匹配（契约测试明确断言了这一点，store/store_retry_test.go:100-104）。

> **版本闸门（2026-10-29 修复，commit 90759e6）。** JSONL 读侧 `load` 曾以 `r.Schema > SchemaV2` 判不支持，而写侧自控制面轮起已接受 SchemaV3（agent/persist_util.go:45），导致 SchemaV3 checkpoint 可落盘但 `Close` 后重开目录即 `ErrUnsupportedSchema`（详见 41-decisions.md S1）。现已将读侧上限对齐 SchemaV3（store/jsonl_file.go:44；jsonl.go:33 契约注释同步修订），并新增回归测试：schema_v3_test.go 钉死「V3 checkpoint 双后端落盘重开可读回」与「伪造 SchemaV4 记录读侧仍类型化拒绝」两个契约。未来出现 SchemaV4 时，此闸门即旧代码拒读新文件的唯一防线。

### 1.4 append-only 日志作为主体：被否决的替代

线程 = 一条只追加的 `Record` 序列，`Seq` 单调编号。为什么不选别的形态：

**被否决：可变状态快照（每轮覆盖写）。** 原地更新意味着崩溃窗口里存在「改到一半」的状态，新旧不可区分，恢复语义无从谈起。append-only 让每个崩溃点都对应日志的一个合法前缀——恢复 = 重放这个前缀，这就是 §2.3 半行修复能成立的结构原因。

**被否决：把多记录事务当唯一正确形态。** `Append` 接受一批多条记录，但 DESIGN.md 明确「JSONL 多记录 Append 不被当作断电事务」（docs/DESIGN.md:122）。契约没有要求后端提供跨记录断电原子性，而是用**前缀幂等**补偿：断电可能让批次的任意前缀落盘，重试同批次时已落盘前缀被逐字节识别并采纳、余下部分补齐（§5.3）。这把对后端的事务性要求降到最低——「单条记录崩溃原子 + 前缀分类重试」即可，一个文件系统就能做到，不需要数据库。

**内核闭包约束顺带决定了第一版只有两个后端。** `store` 在四包内核闭包内、stdlib-only（00-overview.md §2.2 的闭包守卫），能选的持久介质天然是「文件」与「内存」；数据库后端留给宿主自建，而 §5 的扩展契约就是为这天准备的。

## 2. JSONL 后端

### 2.1 文件布局

`NewJSONL(dir)`（store/jsonl.go:69）：目录不存在则创建（0o755），然后做两件防御性的事——

- **符号链接规范化。** `filepath.EvalSymlinks` 把目录解析到物理路径，失败即报错不降级（store/jsonl.go:80-84）：注释写明「Failing open would register a lexical alias under a different key and defeat the one-instance rule」——宁可失败，不许别名绕过单实例规则。
- **单实例登记。** 进程级 `jsonlRegistry`（store/jsonl.go:20）按物理路径登记，同一目录同时只允许一个活实例（§2.4 讲为什么）。

每个线程一个文件：`<url.PathEscape(threadID)>.jsonl`（store/jsonl_ops.go:193），0o644，**首次 Append 才惰性创建**（store/jsonl_ops.go:60）。文件内容是每行一条 JSON 记录——`Record` 结构体直接序列化，行界即记录界。`PathEscape` 承担「调用方 ID 是任意字符串」与「文件名必须安全」之间的翻译；store 永不规范化 ID 本身，agent 侧另加转义后长度 ≤255 的边界检查（第 12 篇 §4.1）。`Delete` 移除整个文件（store/jsonl_ops.go:161-164）。

**为什么一线程一文件而不是一线程一分区/一大文件。** 大文件多线程混排意味着：读一个线程要全文件扫描、删一个线程要原地压缩（违背 append-only）、崩溃修复无法按线程隔离。一文件一线程让每个线程的日志在物理上连续、可独立损坏、可独立删除——代价只是文件数量，对单进程单用户的嵌入场景可接受。

### 2.2 写入时机与 durability

`Append`（store/jsonl_ops.go:16）的完整时序，每一步都有其位置的理由：

1. `checkOpen` + ctx 检查 → `lockThread`（锁内再查一次 closed 与 ctx，见 §2.4）；
2. `prepareBatch`（store/store.go:153）：深拷贝 payload、归一 schema、结构校验——「stored state never aliases caller memory and validation cannot diverge」（store/store.go:149-152）；
3. `planAppend`（store/store.go:180）：对着当前日志分类这次 Append（§5.3）；已全部应用则直接返回 head，**一个字节都不写**（store/jsonl_ops.go:41-43）；
4. 盖章：`Seq` 按位置指派、`RecordedAt` 取当前时刻（store/jsonl_ops.go:45-52），整批序列化进**一个缓冲区**；
5. 首次写入时打开文件（`O_CREATE|O_WRONLY|O_APPEND`）并 **syncDir 一次**（store/jsonl_ops.go:60-66，`syncDir` 在 store/jsonl_file.go:85）——目录项的落盘与内容的落盘是两回事，不同步目录，崩溃后可能剩下「内容都在但文件不存在」的窗口；
6. **一次 `Write` 写整批**（store/jsonl_ops.go:70），然后 **`file.Sync()`**（store/jsonl_ops.go:73）；
7. Sync 成功后才把记录并进内存视图（store/jsonl_ops.go:76）。

核心承诺在类型注释的第一句：「every acknowledged append is fsynced to the file before Append returns」（store/jsonl.go:28）。**为什么每次都 fsync、不批量确认**：ack 是重试分类的地基——「返回 nil 的写入一定活着」。若 ack 过的记录可能在崩溃后消失，调用方就无法区分「没写进去（可安全重试）」与「写过但丢了（重试会怎样？）」，字节同一重试的前提坍塌。批量 ack 换来的吞吐不值得付这个代价（这也是包级反模式清单明令禁止的方向：不得 fsync-skip 或 batch-acknowledge）。

**写失败后的 reconcile。** Write 出错或短写、Sync 失败时，不是简单返回错误：先 `th.load()` 从磁盘重载（`reconcile`，store/jsonl_ops.go:80-87），让内存视图回到与持久字节一致的状态，再返回原始错误。含义有二：其一，即使进程内，一个批次也可能在磁盘上留下**部分前缀**（Write 写了一半出错），内存视图绝不领先于磁盘；其二，随后的重试将对着真实磁盘状态分类（planAppend 的前缀规则接手），而不是对着一个包含「从未落盘字节」的幻觉。重载本身失败时 `errors.Join` 两个错误一起返回。

### 2.3 崩溃一致性：半行修复与内部损坏响亮失败

`load`（store/jsonl_file.go:13）是崩溃后第一次触碰线程文件时执行的修复/校验程序。判据只有一条：**损坏发生在尾部还是内部**。

**尾部的半行（撕裂写）：修复。** 扫描发现最后一行没有结尾换行（断电时 `Write` 只落了半条记录），就截断文件到「最后一个完整行的末尾」，并且**这个截断本身也 fsync**（`truncateAndSync`，store/jsonl_file.go:70，注释「the size change is synced before the repair is considered done」）——修复不能自己成为新的崩溃窗口。为什么敢修：append-only 的结构保证「日志的任何前缀都是合法日志」，断在半行等价于断在那条记录之前，截断只是回到最近的合法前缀。测试 `TestJSONLTornTailRepairedOnReopen`（store/jsonl_test.go:62）模拟半行写入后重开，head 恢复到 2 且后续 Append 正常；`TestJSONLAppendAfterRepairKeepsByteLayoutStable`（store/jsonl_test.go:100）进一步断言「修复 → 重试补齐 → 再重开」之后**字节布局稳定**（无重复行、无残行）。

**内部损坏：响亮失败，绝不修复。** 以下全部 `ErrCorruptLog`（store/store.go:69-71 定义；注意哨兵注释明确「A torn final line is not corruption」）：

| 损坏形态 | 检测点 | 为什么不容忍 |
|----------|--------|--------------|
| 内部行非法 JSON | store/jsonl_file.go:40-42 | 内部字节被动过 = 有东西改写了历史；日志是真相，静默跳过等于伪造一条从未存在过的历史 |
| 空白行 | store/jsonl_file.go:37-38 | 一条被静默丢弃的记录在字节上正是一行空行——必须显影为损坏而不是被吞掉（测试注释原话，store/jsonl_crash_test.go:60-61） |
| 重复记录 ID | store/jsonl_file.go:50-53 | ID 是「永久标识恰好一条记录」的身份；重复意味着身份系统被破坏 |
| Seq 跳号 / 不等于行号 | store/jsonl_file.go:59-61 | 序列即位置；有洞的日志无法做前缀幂等分类 |
| checkpoint 无 payload | store/jsonl_file.go:54-56 | 只有位置没有快照的 checkpoint 是自相矛盾的（§4.3） |
| schema 超出理解 | store/jsonl_file.go:44 | 未来版本必须类型化报错，不许装懂（§1.3；V2/V3 历史缺口已于 90759e6 修复，测试 schema_v3_test.go 钉死） |

`TestJSONLBlankLineIsCorruption`（store/jsonl_crash_test.go:45）的注释点出了空行检测的设计动机：「A silently dropped record would shorten the log; a blank line must surface as corruption instead」——宁可让用户面对一个响亮的错误，也不让日志悄悄变短。

**Delete 也走 load。** 删除前先加载（含崩溃修复）线程（store/jsonl_ops.go:134-136 注释：「a persisted-but-unloaded thread is genuinely removed」），再关句柄、删文件、syncDir。这保证「存在磁盘上但本进程从未读过的线程」也能被真正删除——`TestJSONLFreshInstanceDeleteRemovesPersistedThread`（store/jsonl_lifecycle_test.go:97）钉的就是这个：新实例对一个从未 load 的线程 Delete 后，目录为空，且线程可按 expected=0 重建。

### 2.4 生命周期：单实例注册表与 Close 排空

**为什么一个物理目录只允许一个活实例。** 锁是实例局部的（`JSONLStore.mu` / 每个 `jsonlThread.mu` 都长在实例上）——两个实例指向同一目录会各自持有自己的「日志视图」，交错追加会铸出重复的 Seq。注册表把这个「文档里的单进程限制」变成**强制错误**而不是静默损坏（store/jsonl.go:15-23 注释原话：「the registry turns that documented single-process restriction into an enforced error instead of silent corruption」）。`TestJSONLSecondLiveInstanceOnSameDirectoryIsRejected`（store/jsonl_lifecycle_test.go:11）与符号链接别名测试（:55）钉住这两层。

**Close 的线性化时序**（store/jsonl.go:101-104 注释；实现在 :106-148）：

1. `closed` 原子置位——从这一刻起新操作一律 `ErrClosed`；
2. 排空（drain）：逐个锁住每个线程——等所有**已被接纳**的在飞操作跑完；
3. 关闭全部文件句柄；
4. **只有当注册表项仍指向自己时**才注销目录（store/jsonl.go:132-136）——防止陈旧实例的重复 Close 把新实例的登记抹掉（`TestJSONLRepeatedCloseNeverUnregistersNewerStore`，store/jsonl_lifecycle_test.go:31）。

并发/重复 Close 通过 `drainDone` channel 汇合到同一次排空：任何一次 Close 都不在排空与注销完成前返回（store/jsonl.go:107-113）。实现细节里有一处值得注意的锁序设计：`closed` 用 `atomic.Bool` 而不是普通 bool，是为了线程接纳处可以在只持线程锁的情况下复查它，避免 `th.mu → s.mu` 的锁序反转（store/jsonl.go:49-52 注释）。

**接纳与 Close 竞争的闭环在 `lockThread`**（store/jsonl_ops.go:185-219）：拿 store 锁查/建线程对象 → 释放 store 锁 → 锁线程 → **再查一次 closed**（store/jsonl_ops.go:199-202，注释：「closing the unlock/relock gap between admission and work」）→ 查 ctx → 惰性 load。三组测试钉住整套语义：在飞操作持线程锁期间，目录保持登记（不可能创建替代实例）、Close 不返回（store/jsonl_close_test.go:16）；两个并发 Close 都汇合同一次排空（:64）；与并发 Append 竞争时，每个 Append 要么完整落盘要么 `ErrClosed`，且排空结束后每个成功者都真实持久（:126）。

### 2.5 checkpoint 的落盘格式

在 store 的字节视角里，checkpoint 记录没有任何特殊物理形态：**一行普通的日志记录**，`kind="checkpoint"`、按写入时代的 schema（1/2/3）、agent 铸的 `cp-<head>` ID、payload 是 agent 的快照 DTO。store 侧声明的最小 payload 契约只有一个类型：

```go
type Checkpoint struct {              // store/store.go:95
    History []llm.Message `json:"history"`
}
```

真实快照（canonical history + 冻结 system + run 生命周期上下文 + 控制面投影，`codec_version` 标记）是 agent 的 `agentCheckpoint` DTO，装在 payload 里，store 从不解码——版本矩阵与 codec 演进规则全在第 12 篇 §5.3，此处不重复。store 对格式仅有的两条规则：checkpoint 必须携带非空 payload（写侧 `validateRecord` store/store.go:231-233 拒绝；读侧 load 视为损坏，§2.3 表末行），以及 §4 要讲的定位与遮蔽语义。

## 3. 内存后端

### 3.1 定位

`MemoryStore`（store/memory.go:12，`NewMemory` :18）的注释自述定位：「process-local and loses all data when the process exits; use it for tests and as the zero-configuration backend」（store/memory.go:9-11）。两个场景：**测试**（agent 包全部持久化/结算测试都用它——快、确定、可内省）与**短生命周期嵌入**（宿主想要 `WithThread` 的全部协议行为但不需要跨进程存活：一次 CLI 会话、一个请求作用域的子 agent）。它是契约存在感的证明：`Store` 接口可以在零 I/O 的东西上完整实现，「持久化语义」与「存储介质」真正解耦。

### 3.2 行为等价的机制保证

两个后端**不是两份实现**，而是「共享内核 + 各自的壳」：

- **共享写入内核。** 两个后端的 Append 都在各自的锁内依次走 `prepareBatch`（store/store.go:153）+ `planAppend`（store/store.go:180）。注释写明动机：「so stored state never aliases caller memory and validation cannot diverge」（store/store.go:151-152）、「Both backends share it so retry semantics cannot diverge」（store/store.go:176-177）。校验、幂等分类、ID 唯一性——所有容易在后端间漂移的语义只有一份代码。
- **共享读取语义。** `Latest` 的 checkpoint 切分用同一个 `lastCheckpoint`（store/memory.go:105，从尾部反向扫描），`History` 的 from 边界用同一个 `historyStart`（store/store.go:246），返回副本用同一组 `cloneRecord` / `cloneRecords`（store/memory.go:114，payload 深拷贝）。
- **契约测试族双跑。** 三组契约套件——`exerciseContract`（store/store_contract_test.go:44，序号分配/checkpoint/删除/并发隔离）、`exerciseRetryContract`（store/store_retry_test.go:13，修订匹配/前缀幂等/ID 唯一性/输入校验）、`exerciseHistoryContract`（store/store_history_test.go:13，全量回退读）——都以 `newStore func() Store` 参数化，同时跑在内存（store/store_test.go:5）与 JSONL（store/jsonl_test.go:21）上；schema 行为另有双后端回环测试（store/schema_v2_test.go:8）。**任何「JSONL 是这样、内存是那样」的漂移都会当场红**。这是等价性的机械保证，不是口头承诺。

后端间允许的差异都在契约未承诺的维度上：内存没有 Close/ErrClosed、没有损坏路径（无可损坏之物）、durability 承诺空洞（无进程外存活可谈）；锁粒度也不同——内存是全店一把 `sync.Mutex`（store/memory.go:12-14），JSONL 是每线程一把锁（store/jsonl_ops.go:185 起）。包契约只要求「并发安全；单线程写入串行化」（store/store.go:11-12），两者都满足。另有一个测试缝隙的不对称：内存的 `RecordedAt` 走可替换的 `timeNow` 缝（store/store.go:258），JSONL 直接用 `time.Now()`（store/jsonl_ops.go:45）——纯测试便利，无语义后果。

## 4. checkpoint 作为加速器：store 侧的根据

第 12 篇 §1.3 已讲清定位裁决（「可重建加速器，不是正确性必需品」）与 agent 侧的写入时机、版本纪律；本节只补存储侧的字节根据——**「可丢弃重建」在字节层面靠什么成立**。

### 4.1 `ThreadState` 的切分语义

`Latest` 返回的 `ThreadState`（store/store.go:100-108）把日志切成三段：

- `Checkpoint`：**最新一条** checkpoint 记录（从尾部反向扫到的第一条，store/memory.go:105-110），没有则为 nil；
- `Tail`：该 checkpoint **之后**的记录（`Seq > Checkpoint.Seq`）；无 checkpoint 时整个日志都是 Tail；
- `Head`：下一个可指派的 Seq——即 Append 所期望的修订号。

切分点就是重放边界：调用方（agent 的 `replayThread`，第 12 篇 §5.4）拿快照做基座、只重放 Tail。多条 checkpoint 共存时**只有最后一条生效**（`TestJSONLStoreContract` 的「second checkpoint supersedes the first」子测试，store/store_contract_test.go:141-163）——取代是纯读侧的 positional 规则，旧 checkpoint 行永远留在文件里，`Delete` 是唯一的删除操作。

### 4.2 为什么可以丢弃重建

三个字节层面的事实叠加出「checkpoint 丢了什么都不损失」：

1. **日志从不截断。** checkpoint 记录是普通追加行；写新 checkpoint 不删旧的，`Latest`/`History` 也从不改写或压缩日志。快照与日志永不同步覆写，不存在「快照更新到一半」的形态。
2. **`History` 永不遮蔽。** 接口文档写明它「including records a checkpoint would hide: unlike Latest it never omits a prefix」，并给出存在理由：「It is the fallback read for replay when the newest checkpoint cannot be trusted」（store/store.go:133-137）。`from <= 0` 取全量（记录从 Seq 0 起），`from >= head` 取空（`historyStart`，store/store.go:246-253）。`TestHistoryContract`（store/store_history_test.go:17-78）断言了含被 checkpoint 遮蔽记录的全量返回与 from 边界的精确行为。
3. **无 checkpoint 的 `Latest` 退化为全量。** 整个日志成为 Tail——重放从 Seq 0 开始，重建的是**压缩前**的完整历史。

三者合起来：物理上删掉最新快照（或全部快照），可用信息零丢失——`Latest` 自动退化、`History(0)` 永远兜底；损失的只是加速（重放更长、消耗更多 token）。这正是 DESIGN.md:98「检查点是可重建加速器……日志仍全量保留，checkpoint 不是生命周期真相」的存储侧实现方式。对后端作者的推论见 §5：**不要维护「当前快照」这个可变状态**（那是被否决的可变快照形态的还魂），快照的取代只发生在读侧。

### 4.3 快照边界的一个 guard

「checkpoint 必须携带非空 payload」（写侧 `ErrInvalidRecord`，store/store.go:231-233；读侧 `ErrCorruptLog`，store/jsonl_file.go:54-56）是这个加速器唯一的格式级 guard：一条空 payload 的 checkpoint 声称「在位置 X 有个快照」却不携带快照——读侧无法区分「被截断的 payload」与「写者 bug」，而快照的**位置承诺**（遮蔽 X 之前的全部记录，§4.1）已经生效，容忍它等于让重放基座指向虚空。宁可在加载时报损坏（JSONL 语义下可人工修复），不许半吊子快照上岗。

## 5. 扩展契约：新增一个后端要满足什么

接口文档写下的只是显式承诺；真正完整的契约在**测试断言**里。三组 exercise 套件 + agent 侧守卫测试共同构成新后端的达标线——本节把它们反推成清单。最省事的验证方式：把新后端接进三组参数化套件（`exerciseContract` / `exerciseRetryContract` / `exerciseHistoryContract`，签名都是 `func(t, newStore func() Store)`）直接跑。

### 5.1 硬性要求清单

**（1）Seq 的指派与不变量。** Seq 由 store 在 Append 时指派、从 expected 起连续编号（`TestContract`「append assigns sequential sequences from expected」，store/store_contract_test.go:47-71）；稠密、从 0 起、值即位置（JSONL 在**读回时**校验 `Seq == 行号`，store/jsonl_file.go:59-61——新后端即使不存 Seq 也必须在返回时重建出满足这条的值）。返回的 head = `expected + len(batch)`，若日志已更长则取当前 head（`planAppend`，store/store.go:209-211）。

**（2）乐观并发：stale expected 一票否决且零副作用。** 冲突的 Append 不写任何字节（store/store_contract_test.go:72-88 断言冲突后状态未变）。空批次是纯修订探针：不写、expected 不超前就返回当前 head（store/store_retry_test.go:179-193）——有调用方靠它无副作用地读 head。

**（3）全内容幂等（k 前缀规则）。** 设日志已有 k 条落在 `[expected, expected+k)`：前缀必须与批次逐项全等（ID/Kind/Schema/Payload，字节级）才被采纳；余下记录的 ID 必须全新；其余一切 `ErrRevisionConflict`（接口文档 store/store.go:122-129；实现 store/store.go:180-214）。对应断言家族：同内容重试幂等不重复（store/store_retry_test.go:17-37）、半应用批次重试补齐（:39-61）、日志已前进后的陈旧批次重试返回当前 head 且不写（:62-84）、payload/_kind 改动的重试冲突（:86-106）、同 ID 换位置冲突（:108-116）。

**（4）ID 唯一性的两种拒绝。** 批内重复 ID → `ErrInvalidRecord`（store/store_retry_test.go:118-123）；ID 已在日志他处使用 → 同样 `ErrInvalidRecord` 而非冲突（:125-133）——这是「畸形写入」，不是「过时修订」，错误类型不可混。

**（5）批次事务性：不要求断电原子，要求前缀可分类。** DESIGN.md:122 明确多记录 Append 不是断电事务；进程内也一样——JSONL 的 Write 半途失败会留下持久前缀（§2.2 reconcile），契约的补偿是前缀幂等。因此新后端**可以**把整批放进一个数据库事务（此时可落盘前缀只有 0 或全部，是 k 规则的特例，契约同样接受），也**可以**逐条落盘；但单条记录的崩溃原子性不可妥协，且无论断在哪里，字节同一的重试都必须能按 §5.1(3) 分类。

**（6）系统边界的结构校验，且在锁内、分类前完成。** kind 合法性（未知 → `ErrUnknownKind`）、ID 非空、payload 是合法 JSON、schema 区间与 per-kind 钉死、unset 归一为 V1（`validateRecord` + `prepareBatch`，store/store.go:216-241/:153-171；断言 store/store_contract_test.go:188-196、store/store_retry_test.go:135-152/:154-161）。深拷贝隔离双向成立：写入方向「caller payload mutation after append is invisible」（store/store_retry_test.go:163-177，prepareBatch 的深拷贝负责）、读取方向「history returns copies the caller cannot use to mutate the store」（store/store_history_test.go:88-103，cloneRecords 负责）。

**（7）读契约的边界形态。** 未知线程：`Latest` 返回空 `ThreadState{Head:0}`、`History` 返回空——**不是错误**（store/store_contract_test.go:89-99、store/store_history_test.go:80-86）；线程的存在性没有独立的可观察 API，head=0 就是全部事实。`History` 永不遮蔽 checkpoint 覆盖的前缀（§4.2）；`from` 越界取空不报错（store/store_history_test.go:47-78）。

**（8）Delete 的完整语义。** 删除全部记录、幂等、未知线程等价删空线程、删除后可用 expected=0 重建（store/store_contract_test.go:165-186）。惰性加载的后端必须像 JSONL 一样**先加载再删**（store/jsonl_ops.go:134-136）——否则「存在介质上但本进程没碰过的线程」成为僵尸。

**（9）ctx 纪律。** 每个方法入口检查 `ctx.Err()`，**拿到锁之后、变更之前再检查一次**（store/memory.go:24-30 等；断言 store/store_contract_test.go:198-208、store/store_history_test.go:107-117）——取消必须发生在任何变异之前，锁等待期间的取消不能被吞。

**（10）并发与「可作 map 键」。** 并发安全、单线程写入串行化（store/store.go:11-12）；跨进程写者 v1 不支持。容易被忽略的一条来自 agent 侧：运行所有权注册表以 `（Store 值, 线程）` 为键做 map 查找，**Store 的动态值必须可比较**——指针式实现天然满足；含 map 的值接收者会被 agent 以 `ErrStoreNotComparable` 类型化拒绝而不是 panic（守卫测试 agent/persist_store_guard_test.go:80-88；DESIGN.md:102「Store 值必须可比较（指针式实现，两个内置后端均满足）」）。新后端请返回指针。

**（11）ack 即持久。** Append 返回 nil ⇒ 记录在进程崩溃后存活（文件后端 = fsync，数据库 = 提交完成）。不得批量确认、不得「先 ack 后刷盘」——这是 §2.2 全部重试语义的地基，对任何介质后端同样成立。

### 5.2 agent 侧守卫对 store 的隐含要求

两个守卫测试把「agent 依赖 store 的哪些未成文行为」钉成了可执行规格：

- **模糊失败的采纳。** `ambiguousStore`（agent/persist_store_guard_test.go:34-50）模拟「记录已真实落库但 store 返回错误」；`TestGeneratedIDReservationAmbiguousFailure`（:56-74）断言 agent 的字节同一重试被 store 识别为已应用、采纳同一 run 继续跑——不产生新 ID、不重复记录、不搁浅 active run。对新后端的含义：**重试分类必须基于已落盘内容，绝不基于「上次调用说了什么」**；网络数据库后端应把记录 ID 当幂等键实现这一点。
- **可比较性守卫。** `TestNonComparableStoreIsRejected`（agent/persist_store_guard_test.go:80-88）：不可比较的 Store 值得类型化错误而非 panic——§5.1(10) 的测试面。

### 5.3 幂等分类算法本身就是共享代码

值得单独强调：新后端不需要（也不应该）自己实现前缀分类——两个内置后端共用 `planAppend`（store/store.go:180）与 `prepareBatch`（store/store.go:153），包内反模式清单明令「Do not add backend-specific append semantics — extend planAppend/prepareBatch so both backends stay identical」。若新后端需要不同的分类规则，正确动作是**扩展共享函数**，让全部后端一起获得新语义；在自家后端里写一份私有变体，等价于在契约上凿出第二个真相。

## 6. 测试即契约

| 测试 | 钉住的契约 |
|------|------------|
| store_contract_test.go:44 `exerciseContract` | §1.1/§5.1：序号分配、checkpoint 切分与取代、删除语义、未知 kind/空 ID、ctx 取消、并发隔离——双后端共用 |
| store_retry_test.go:13 `exerciseRetryContract` | §5.1(3)(4)(6)：全内容幂等家族、ID 唯一性、schema/输入校验、双向隔离、空批次探针——双后端共用 |
| store_history_test.go:13 `exerciseHistoryContract` | §4.2/§5.1(7)：全量回退读、from 边界、未知线程、副本返回 |
| store_test.go:5 / jsonl_test.go:21 | 三套件分别在内存/JSONL 上接线——等价性的机械保证 |
| jsonl_test.go:62 撕裂尾修复 / :100 修复后字节稳定 | §2.3 尾部半行截断修复与重开布局不变 |
| jsonl_crash_test.go:13/:45/:78/:96/:114 | §2.3 内部损坏族：坏行、空行、Seq 跳号、未来 schema、重复 ID 全部 `ErrCorruptLog`/`ErrUnsupportedSchema` |
| jsonl_close_test.go:16/:64/:126 | §2.4 Close 排空：在飞操作期间目录保持登记、并发 Close 汇合、竞争 Append 要么落盘要么 ErrClosed |
| jsonl_lifecycle_test.go:11/:31/:55/:97 | §2.1/§2.4：单实例、重复 Close 不注销新实例、符号链接规范化、未加载线程可删 |
| schema_v2_test.go:8 | §1.3：控制面 kind 的 schema=2 钉死与双后端断电回环 |
| agent/persist_store_guard_test.go:56/:80 | §5.2：模糊失败的幂等采纳；不可比较 Store 的类型化拒绝 |

## 7. 锚点速查

| 符号 | 位置 | 职责 |
|------|------|------|
| `Store` | store/store.go:113 | 四方法契约：Append :127 / Latest :131 / History :140 / Delete :146 |
| `Record` / `Checkpoint` / `ThreadState` | store/store.go:83 / :95 / :100 | 记录信封；最小快照载荷；切分后的读取视图 |
| kinds / schema 常量 | store/store.go:41-53 / :31-35 | 11 种 kind；SchemaV1-V3 |
| 哨兵错误 | store/store.go:59-78 | ErrRevisionConflict :61 / ErrUnsupportedSchema :64 / ErrCorruptLog :69 / ErrUnknownKind :72 / ErrInvalidRecord :76 |
| `prepareBatch` / `planAppend` / `validateRecord` / `historyStart` | store/store.go:153 / :180 / :216 / :246 | 共享写入内核：归一+校验、幂等分类、结构规则、from 边界 |
| `timeNow` | store/store.go:258 | 测试时钟缝（内存后端使用） |
| `NewJSONL` / `Close` / `ErrClosed` / `jsonlRegistry` | store/jsonl.go:69 / :106 / :13 / :20 | 构造与符号链接规范化；线性化关闭；单实例登记 |
| `JSONLStore.Append` / `reconcile` | store/jsonl_ops.go:16 / :80 | 盖章-单写-fsync 时序；写失败后从磁盘重载 |
| `JSONLStore.Latest` / `History` / `Delete` / `lockThread` | store/jsonl_ops.go:91 / :119 / :137 / :185 | 切分读；全量读；先加载再删；接纳与 closed 复查 |
| `load` / `truncateAndSync` / `syncDir` | store/jsonl_file.go:13 / :70 / :85 | 崩溃修复与损坏校验；持久截断；目录同步 |
| `NewMemory` / `MemoryStore` | store/memory.go:18 / :12 | 进程内零配置后端 |
| `lastCheckpoint` / `cloneRecord` / `cloneRecords` | store/memory.go:105 / :114 / :120 | 尾扫最新快照；读侧深拷贝（双后端共用） |
| agent 侧守卫 | agent/persist_store_guard_test.go:56 / :80 | 模糊失败采纳；Store 可比较性 |
