# codemode 设计契约

本文记录 codemode 卫星的设计契约：它是什么、五项已裁决的决策、核心机制如何运作、实现与测试的覆盖边界，以及接线者需要的默认值与前置条件（见 §7 能力与预算与 §8 测试覆盖）。设计基线：go-agent v0.4.0（6f96b1f）；goja 固定源码 f493a2b600d62d18dc5ee168cd1178ae635b0470。完整推导过程（三轮对抗评审、13+5 项发现、delta-2 复审）在评审档案中，本文只收结论。

## 1. 这是什么、解决什么

模型逐个调用工具时有三笔固定成本：每个工具的 JSON Schema 全量随每轮请求广告；每次调用单独一轮往返；工具的完整返回结果全文写进对话历史。工具数量大或批量处理数据时，这三笔账撑爆上下文。

codemode 把三笔账一起省：模型只看到一个 `run_code` 工具，写一段 JavaScript，脚本在进程内的 goja 解释器里运行，通过受控 Host 桥调用业务工具；脚本自己过滤汇总，只有 `return` 的终值（加显式挑选的图片）进入模型历史。2026-11-16 owner 定调：这是 agent runtime 底座的能力面，按底座逻辑取舍，不需要应用场景来论证。

## 2. 五项已裁决决策（2026-11-26，全部按推荐执行）

1. **工具面（D0）**：独立 modelRegistry。模型侧只剩 run_code，原工具不能直接调；紧凑声明常驻 Description，预算 4096 runes，超限硬错。备选 additive 已否决：它只省中间结果和往返，不省 schema 广告。
2. **审批粒度（D10）**：双层。整段代码由内核循环批一次（`RequiresApproval=true`）；代码内每个敏感工具逐个审批，读当次调用 live 句柄的元数据，嵌套审批串行。依据是内核既有逐工具 fail-closed 契约——卫星若不保留它，等于造了一条绕过内核策略的新路。Microsoft 的捆绑审批是行业主流简化项，已知取舍。
3. **超时语义（C）**：有界等待。三个独立时钟（60s 总执行 / 1s 等 VM 退出 / 5s 等宿主排水）；到点返回软错误并如实标注清理状态，未退出的工作继续归 CodeMode 监督、占住 MaxLiveRuns 槽位，Close 等真实退出。
4. **晚到硬错误（D12）**：首个终因优先。终止原因在同一短锁内首次登记选定；之后到达的一切硬错误只记诊断，包括响应尚未返回的排水窗口。理由是停止、清理、响应共享一个稳定原因。
5. **投影（D11）**：Data 只给脚本；模型只拿 `return` 终值加显式挑选的图片；外层 `Data` 恒为 nil；没有"全部中间结果兜底透传"的开关。

行业对照（六项目固定 commit 源码调研）确认五个推荐都在行业包络内；审批与晚到错误两处比主流严，偏离点有专项披露。

## 3. 交付形状与移动部件

卫星包 `github.com/dailz1/go-agent/codemode`，内核四包零改动，kernel-closure-guard 零改动，唯一第三方依赖是固定版本的 goja。八个源文件、四个部件组：

**执行器协议（executor.go）**：`Executor` 三方法接口（Profile / RenderAPI / Run），语言无关；下游可换 Starlark、子进程等后端，v1 只交付并维护 goja 实现。`Host` 接口三个方法——Submit（准入或立即拒绝）、Reserve（owner 阻塞取下一个已预留投递的完成）、FinishDelivery——把"排队不等于获准投递"写进契约。

**Host 桥（bridge.go）**：执行记录持有准入门、投递门、计数器（callsSubmitted / pendingCalls / deliveriesInFlight / hostOutstanding）、宿主 worker 池、审批槽、图片表和 stdout 预算。`e.mu` 是停止转换与投递预留共享的短临界区，绝不在持锁状态下执行用户代码、工具调用或 resolver 入口。

**goja 适配器（goja.go）**：单 owner 循环。创建 VM 后、运行任何代码前注册非阻塞 Interrupt 并禁用所有解析入口的 source maps（否则 goja 解析器会按 sourceMappingURL 读文件）；用提前保存的原始 `Promise.prototype.then` 安装宿主 continuation；每完成一次预留投递就重入 runtime 让 promise jobs 跑到静止（`Runtime.leave()` 在顶层返回时清空 jobQueue）；终值用增量编码器在 OutputBytes 预算内逐项写出，超预算置 ValueOmitted，不构造整串再截断。

**生命周期（codemode.go / tool.go）**：不可变 generation + 绑定它的 `run_code` wrapper；Refresh 在生命周期锁内完整构建 g2、确认所有权、Replace 单点发布。supervisor 驱动一次执行到响应结算，cleanup_incomplete 时转后台继续监督。

## 4. 授权路径（裁决 B，逐调用）

1. worker 从队列取出任务；停止先到则该调用 `cancelled_before_start`，不计副作用。
2. `source.Get(name)` 一次固定句柄 t；同一 t 上读一次 Info 并拷贝元数据。
3. 校验 info.Name 与请求名一致；把当次 Parameters 规范编码与 generation 指纹比较，不一致返回 `catalog_stale` 软错不执行。RequiresApproval 从 live Info 读，从不读 generation 缓存——权限变更即时生效。
4. fail-closed 审批：无回调即拒绝；回调拿副本，改参无效；panic 归宿主集成硬错误。
5. 短锁内检查停止门并登记 started（启动执行的线性化点）；解锁后执行已授权的旧 t。批准期间发生的 Replace 仍执行被批准的旧句柄，下一次调用才解析到新句柄。

## 5. 投递预留协议（D13 + delta R1）

worker 完成后只把纯数据完成记录送入 inbox——入队不等于获准投递，worker 从不触碰 resolver。VM owner 调 `Reserve()`：在停止门共用的短锁内检查投递门，门开则登记唯一预留（DeliveriesInFlight++、pendingCalls--），门关则丢弃该数据继续等停止。预留成立后，即使门随后关闭，这次完成仍必须进入它的那一次 resolver 入口；入口在锁外执行，可被 Interrupt 立即打断。入口返回或异常退出后递减计数；停止后的投递只产生 `late_delivery_finished` 元数据。顶层 continuation 触发即 `ScriptSettled`：pendingCalls 归零进入正常收尾，否则选 `unawaited_calls` 终因并停止运行。完成判据是 continuation 已触发，不是队列为空。

## 6. 错误模型（D12）

工具级软错（Status=error、denied、not_found、catalog_stale、单调用超限）脚本可 catch，结构化 result 保留；编译错误、未捕获异常、result_encoding、总量耗尽是终局软失败；deadline、取消、unawaited 是带原因和清理状态的软错误；宿主侧 Go 硬错误在终因尚未选定时向上传播中断 Agent 轮次，终因选定后只作诊断。全部外层结果 Go error 为 nil（硬传播除外），与普通工具软错契约一致。

## 7. 能力与预算（D5/D6）

goja 三项 guest 资源能力（步数、内存、强制停止）均不支持，Profile 如实声明为 false；在 Config 上配置这些域直接 Connect 硬错，绝不模拟成软上限。数值预算全部按 UTF-8 字节或图片 payload 字节计（DescriptionBudget 按 rune）；优先级是先局部上限后域总预算取小；最终产物依次分配终值 Content、显式图片、stdout，另预留 256 字节给省略 marker。禁直接 I/O 含所有解析入口的 source maps；这些是能力声明，进程安全隔离是另一档承诺（需要子进程/容器后端）。

## 8. 测试覆盖

codemode 包内白盒测试覆盖：schema 渲染全规则表（D8）与退化诊断；端到端脚本流（数据进脚本、终值进历史、外层 Data=nil）；拒绝可 catch；陈旧 catalog_version 拒绝不携带他代声明（D2-1）；脚本错误与编码失败软错；unawaited 终局；超时软错带清理状态；call_limit 终局；Refresh 代际隔离与所有权丢失；Replace 时授权（权限变更即时生效、批准期间仍执行旧句柄）；审批回调改参无效；schema 变更 catalog_stale；Promise.all 真实并行；迟到完成只进元数据；图片选择、跨执行引用拒绝、MaxImages 上限；真实 Agent loop 的双层审批与历史隔离（中间结果不进 history）；Close 尊重用户 Replace；观察者时序（queued→approving→started→finished、单调 seq）。全部在 `go test -race` 下通过。examples/codemode 用 ScriptedProvider 演示完整流程（P1 触发二次审批，P2 不触发）。

## 9. 与内核的边界

satellite 只通过公开 API 触达内核：Registry 的 Get/List/Replace/Unregister、Tool 接口、agent 的 WithApprovalFn/WithDynamicTools。私有 registryEntry.paramsJSON 不可达也不需要——指纹来自公开 List 的深拷贝 marshal。未伪造内核 ToolCallEvent/ToolResultEvent 或 call ID；卫星观察事件带自己的 ExecID/CallID/EventSeq。若未来要求共享内核执行入口、原生父子身份或独立于模型历史的持久审计，那是需要另行裁决的内核 seam，本版明确不做。harness 不接线；内核闭包禁第三方依赖不受影响。
