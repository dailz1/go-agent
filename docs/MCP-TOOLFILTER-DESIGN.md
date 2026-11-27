# mcpbridge 工具过滤设计契约

冻结版本 v1（2026-11-26）。范围裁决"可以"、设计默认收口"开始"均为 owner 2026-11-26 给出。基线 adce356。

## 1. 这是什么、解决什么

mcpbridge.Connect 把远端 MCP server 分页发现的全部工具平铺注册进 Registry，没有过滤面；工具多的 server 整体进入每轮请求广播。本契约给连接器层加一道配置时过滤：被滤掉的工具不注册，模型从头到尾不可见。业界先例：Gemini CLI `includeTools/excludeTools`、Codex CLI `enabled_tools/disabled_tools`（2026-11-26 调研）。

## 2. 公开 API

```go
// ToolFilter reports whether a remote MCP tool should be registered into the
// local registry. It receives a copy of the remote tool as discovered by
// tools/list; treat the input as read-only — mutating it has undefined
// effects on what gets registered. The final local name is Namespace +
// "__" + Tool.Name. Return false to skip the tool: it never reaches conflict
// checks or schema projection, but required name-validation errors are
// still recorded. Once a tool is rejected, its name stays excluded for the
// lifetime of the Bridge, even if the server later changes the tool.
type ToolFilter func(tool *sdk.Tool) bool

// WithToolFilter sets the connection-level tool filter. The filter applies
// at Connect and again at every Refresh, and rejections are retained: a name
// rejected at any listing stays out for the lifetime of the Bridge, even if
// a server-side change would later make the predicate accept it. A nil
// filter (the default) keeps current behavior: every discovered tool is
// registered. When used through Manager, the predicate runs while the
// manager holds its own lock; it must not call back into the same Manager
// (Snapshot, OwnerOf, Refresh) or use precomputed state instead.
func WithToolFilter(filter ToolFilter) Option
```

sdk 类型进公开签名沿用 `Connect(transport sdk.Transport)` 的既有先例。

## 3. 行为契约

1. 生效点在 prepareTools 的逐工具循环内：名字合法性检查之后、schema 投影与 wrapper 构建之前。被过滤工具不参与查重，与已注册名的潜在冲突不产生 NameNotRepresentableError。
2. Connect 与 Refresh 使用同一谓词，且拒绝结果按名字保留：任何一次列举中被拒绝的名字，在 Bridge 生命周期内持续排除，服务器后续变更描述也不会使其进入注册。失败 Refresh 的拒绝决定照常保留（注册表与 owned 集不动），已发布 wrapper 的移除发生在下一次成功 Refresh；两次成功 Refresh 之间已进入执行的调用仍可能完成一次。保留判定先于谓词调用（已拒名字不再触发谓词），后于名字合法性校验；被滤掉的工具不参与查重。谓词输入按只读对待。
3. 谓词拒绝全部工具时 Connect 成功返回，Bridge 持有零工具（与远端无工具的既有路径同形）。
4. 默认零行为变化：不设置 WithToolFilter 时注册行为与现状逐字节一致，现有测试全部保持绿色。
5. 可观测性：每次连接结束在 slog Debug 记一条汇总（过滤掉 N/M 个），不逐条记录。汇总日志在 Manager 持有其锁的位置同步输出：日志处理器不得回调同一 Manager（Snapshot/OwnerOf/Refresh），否则死锁。

## 4. 测试覆盖

- 谓词拒绝部分工具：List 不含被滤名，其余照常注册。
- 默认无过滤器：现有全部 mcp 测试零改动保持绿色。
- Refresh 重过滤：Connect 滤掉的工具 Refresh 后仍不在；Refresh 新增的远端工具同样过谓词。
- 全滤空：Connect 成功、owned 为空、Close 正常排水。
- 过滤先于冲突检查：被过滤工具即使会与现有名字冲突也不产生错误。

## 5. 边界

改动仅限 mcp/ 包（options.go、bridge.go、refresh.go 路径与测试）；内核四包、agenttest 录制语法、kernel-closure-guard 零影响。本契约落在独立新文档，不修改 docs/MCP-DESIGN.md（2026-10-29 专项文档保护协议）。
