# toolsearch 设计契约

冻结版本 v1（2026-11-26）。基线 adce356。范围裁决"可以"、设计默认收口"开始"均为 owner 2026-11-26 给出；设计简报经对抗评审（REVISE→修订→delta-1 APPROVE）后按本文实现，实现评审对照本文与源码。

## 1. 这是什么、解决什么

模型每轮请求携带全部工具 schema，工具越多定义账越大、选择准确率越低。toolsearch 把低频工具放进私有 source registry，模型初始只看到一个 `tool_search` 入口；搜索命中后，命中的工具以 wrapper 形式注册进 exposed registry，下一轮起以完整 schema 广告，按普通 tool calling 调用。接线与 codemode 同构（source 私有池、exposed 模型侧），两个卫星可共享同一个 exposed。

## 2. 已裁决范围（2026-11-26）

1. 卫星层新增包 `github.com/dailz1/go-agent/toolsearch`，内核四包零改动；内核 seam 说明引用 docs/CODEMODE-DESIGN.md 第 9 节，不另立。
2. 检索只用 BM25（stdlib 实现），`mode` 参数留槽只接受 `"bm25"`，无 regex 检索。
3. 手动触发：宿主决定 source/exposed 分池，搜索工具常驻广告；无自动触发（v2 backlog，可锚 modelmeta 窗口 + provider usage）。
4. 每次装载默认 top-5（WithLimit 可配，1..100），v1 不卸载不淘汰；MaxLoaded 默认 256 硬限，满额拒绝新增保留旧工具。

设计默认（入口名冲突显式失败、空结果为成功空数组、重复命中幂等、权限冻结失配 stale、恢复指引写入 description 等）随 owner"开始"裁决收口为本文契约，不再逐条呈报。

## 3. 公开 API

```go
package toolsearch

import (
	"context"
	"log/slog"
	"sync"

	"github.com/dailz1/go-agent/tool"
)

const DefaultToolName = "tool_search"

type Option func(*config)

type config struct {
	toolName     string
	limit        int
	maxLoaded    int
	catalogLimit int
	logger       *slog.Logger
}

type Search struct {
	mu      sync.Mutex
	source  *tool.Registry
	exposed *tool.Registry
	cfg     config
	owned   map[string]*forwardTool
}

func Connect(ctx context.Context, source, exposed *tool.Registry,
	opts ...Option) (*Search, error)
func WithToolName(name string) Option      // 默认 tool_search，须匹配 [A-Za-z0-9_-]{1,64}
func WithLimit(n int) Option               // 默认与上限一体，1..100，默认 5
func WithMaxLoaded(n int) Option           // 默认 256
func WithCatalogLimit(runes int) Option    // 默认 65536
func WithLogger(logger *slog.Logger) Option // 默认 slog.Default；nil 视为丢弃输出
func (s *Search) Name() string
```

模型侧参数与结果类型：

```go
type searchArgs struct {
	Query string `json:"query"`
	Limit *int   `json:"limit,omitempty"`
	Mode  string `json:"mode,omitempty"`
}

type match struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	NewlyLoaded bool   `json:"newly_loaded"`
}

type searchOutput struct {
	Matches []match `json:"matches"`
	Message string  `json:"message"`
}
```

配置校验在 Connect 注册搜索工具之前完成：数值选项显式零值或负数（Limit 另含 >100）返回 Go error 且 exposed 零变化，省略选项才取默认值；`source==exposed`、source 与 exposed 已有名重叠、source 含搜索工具名均拒绝。搜索结果 Content 承载 searchOutput 的 JSON 文本，Data/Images 为 nil。

## 4. 检索规格（BM25）

- 文档 = `Name + "\n" + Description`，两字段同权；Parameters 不参与打分。
- tokenizer：Latin 转小写后按空白切词（不拆标点/下划线/camelCase）；连续 CJK 字符（unicode Han、Hiragana、Katakana、Hangul）生成相邻字符 bigram，CJK 与其他字符交界切开，单独一个 CJK 字符不生成 token；query 同规则，无法生成 token 返回参数软错误。
- 打分：`k1=1.2`、`b=0.75`，query token 去重；`idf = ln(1 + (N - df + 0.5)/(df + 0.5))`，累计 `idf * tf*(k1+1)/(tf + k1*(1-b+b*dl/avgdl))`；空目录或全空文档直接零命中。
- 排序：分数降序，同分按工具名升序；只返回正分候选。已加载工具参与排名但不补齐、不重复注册，`NewlyLoaded=false`。
- 目录预算 CatalogLimit 计算整份 source 的名字、description 与每项两个分隔字符；超限 search 软失败零变化。返回结果中每项 description 折叠空白后最多 160 runes，超长省略号收尾；打分始终用完整 description。

## 5. 装载顺序与 wrapper

一次 search 依序执行，发布成功后才报告已加载：

1. 解析参数并检查 context；拒绝空 query（TrimSpace 后）、类型错误、不支持的 mode、越界 limit。
2. 取加载锁后再查 context；读 `source.List()`，查目录预算，按名字与 description 建当次 BM25 数据。
3. 按分数降序取前 limit 个（已加载工具参与排名，不补齐）。
4. 检查候选在 exposed 中的所有权与新增数量（MaxLoaded），构造新 wrapper 或元数据刷新 wrapper。
5. Apply 前最后检查 context；用一次 `exposed.Apply` 原子发布（removed 仅用于同名 wrapper 更新）。
6. 更新 owned 表、释放锁、锁外记一条 `toolsearch loaded` 日志（属性仅 `tool_names`、`refreshed_names`、`loaded_total`），返回结果；零变化跳过 Apply 与日志。

wrapper 冻结装载时的 source ToolInfo 深拷贝（来自 source.List，含 RequiresApproval），Info 返回该副本（调用方不得变异，与全仓 Info 只读惯例一致）。Execute 先 `source.Get(name)` 固定当次句柄并读取 Info，与冻结副本的规范化 JSON（对象键序与数字书写形式不敏感、数值按精确值比较，如 1 与 1.0 等价、大整数不丢精度）比较：任一语义变化返回 `catalog_stale` 软错误并指引重搜，业务 Execute 不运行；比较通过则对同句柄调用 `Execute(ctx, args)`，Content/Data/Images/Status/Go error 原样传递。source 同元数据替换实现视为宿主授权，直接转发新句柄；仅关键词载体内部的成员顺序重排不算变化。模型的 limit 参数接受 JSON 数字的整数形式（1.0 等价 1），非整数拒绝。两条边界如实披露：同一条模型响应内，重搜对已加载工具的元数据刷新可能在该响应的后续调用中提前生效，其参数按旧 schema 生成时由源工具自身校验兜底；RequiresApproval 冻结为 true 的 wrapper 在宿主收紧权限后，内核审批门会先于 stale 检测拒绝调用，此时模型收到的是审批错误，重搜后按新权限走审批。

## 6. 错误模型

- 搜索软错误（返回 `tool.NewErrorResult`，Go error 为 nil）：参数错误（空 query、mode 非法、limit 非法、无法分词）、目录超限、MaxLoaded 超限、命名冲突与所有权丢失（错误说明冲突名字，不静默夺回）。
- 成功空态：无匹配返回成功空 `matches` 数组，message 为 `No matching tools; try different task keywords.`；空 pool 为 `No deferred tools are available.`。
- source 删除已加载名字后调用 wrapper：软错误，内容含缺失名字并指引调用搜索工具（用配置后的搜索名）重搜。业务工具自身的软错误与 Go error 原样向上传递。
- 配置错误走 Connect 的 Go error，exposed 零变化。

## 7. 截断与恢复

内核 truncateToolResult 超预算时只改 Content（marker 插在保留首尾之间），可能破坏搜索 JSON 的结构；已装载集合的权威状态是 exposed registry，WithDynamicTools 下下一轮仍从 `exposed.List` 广告全部已加载工具的完整 schema。常驻工具 description 声明三条恢复规则：结果文本可能截断、以下一轮广告 schema 为准、历史工具报 not found 或 stale 时回到本搜索工具重搜。搜索不执行业务工具。

pool、检索数据、loaded 集合不写入 Store；重建实例后从只含搜索工具的 exposed 重新开始，宿主负责重建 source。ResumeThread 对未记录结果的中断调用补 outcome unknown，重搜不改变该语义。

## 8. 测试覆盖（验收表）

| 场景 | 必须观察到的结果 |
| --- | --- |
| 首轮与下一轮 | 首轮只有搜索入口；search 加载后下一轮才广告完整 schema，默认最多五项 |
| 结果截断 | 真实 Agent + 小 context window + 长搜索结果：Content 中间出现 marker；exposed 仍保留全部命中，下一轮广告完整 schema；缩小 limit 重搜不重复注册 |
| 取消时点 | 取锁后与构建完成后观察到取消则零变化；最后检查通过后取消，允许 Apply 完成且不回滚，owned 与已发布 wrapper 一致 |
| 配置边界 | CatalogLimit/MaxLoaded 的 0、负数，Limit 非正数与 >100 均 Connect 报 Go error，exposed 零变化；省略选项才用默认值 |
| 检索确定性 | Latin 空白切词、CJK bigram、同分按名字排序；只返回正分命中 |
| 空结果与输入 | 空 pool、零匹配均成功空数组；非法 query、limit、mode 为软错误且无注册变化 |
| 重复、预算与冲突 | 重搜不增计数；元数据更新保持名字；预算或冲突使整批零变化 |
| 审批 | RequiresApproval 保留，无 callback 不执行；权限变化先 stale，重搜后按新权限审批 |
| source 生命周期 | 同元数据 Replace 转发新句柄；schema/description 变化拒绝旧调用；删除后软错误指引重搜 |
| 调度与外部写入 | 并发 search 不重复注册；同轮未知业务调用不承诺成功；外部失去所有权后不覆盖 |
| 恢复 | 历史保留，重建实例不恢复 loaded；旧名新调用软失败，重搜恢复；中断结果仍 unknown |
| 组合与录制 | 与 codemode 两种安装顺序都保留两入口，加载不破坏 run_code；version 3 录制可回放 |
| 宿主误接线 | source==exposed 初始化失败；未开 WithDynamicTools 时本次 Run 广告不更新 |

## 9. 与内核的边界

- toolsearch 生产代码只导入 stdlib 与 `github.com/dailz1/go-agent/tool`；kernel-closure-guard 无需修改。
- agenttest 录制语法不变：搜索调用与结果沿普通 tool call/result 表达，无新 chunk、AgentEvent、Store record 或 recording version。
- 宿主义务：`agent.WithDynamicTools()` 必开，漏开则本次 Run 广告不更新；对 satellite 拥有名字的外部修改须与装载发布串行；同一条模型响应内"先 search 再调用未加载工具"不承诺生效。
- 无 Close/Refresh/卸载接口：无后台资源，生命周期与宿主 registry 一致。
