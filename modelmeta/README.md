# modelmeta

`modelmeta` 提供 models.dev 派生的模型上下文窗口目录。离线启动用随包快照，需要新数据时显式调用 `Fetch`。目录构造后只读，可并发查询。生产代码仅依赖标准库和 `llm`，内核及适配器不导入本包。

## 来源和覆盖

一次更新固定取得两个公开端点，按下表合并。下载不需要登录，也不发送 Authorization、Cookie 或条件请求头。

| 范围 | 来源 | 规则 |
| --- | --- | --- |
| 除 `openai-codex` 外的 provider | `https://models.dev/api.json` | 只接受 v1，忽略 v2 的普通 provider |
| `openai-codex` | `https://models.dev/experimental/v2.0/api.json?include=openai-codex&type=chat` | 只接受显式加入的实验 v2，忽略 v1 的同名 provider |

同名模型在不同 provider 下分别保留。被采用范围内，provider/model 的 `id` 与对象键冲突会使整次更新失败。完整 JSON 中重复对象键同样报错，不采用最后一个值。v2 的其他 provider 不参与窗口结构校验。未知价格、模态、能力等字段不导入。

只取 `limit.context` 的正整数，最大为 2,147,483,647。缺失、null、非整数、非正数和超限值不登记，并计入 `SkippedModels`。不根据 input/output 推算窗口。错误容器、损坏 JSON、尾随第二份 JSON、无有效窗口，以及 v2 缺失 Codex 或没有有效 Codex 窗口，都使整次更新失败。

2026-11-20 的取证与随包快照包含 325 个 provider、9,877 条窗口。Codex 专用目录当日只有两项：`gpt-6.1-terra`、`gpt-6.1-sol`，各为 1,050,000 tokens。这表示该公开目录的覆盖范围，不表示所有账号可用模型。代码不固定未来的 Codex 名称或条数，只要求至少一个有效窗口。实验 v2 可能变更结构；失败时保留旧目录，继续按旧年龄判断。

来源取证详见项目设计简报；随包数据中的 `sources` 记录完整响应的 SHA-256、ETag、URL、取得并验证结构的时间及跳过数量。取得时间不表示逐条人工核实，上游模型的 `last_updated` 不参与年龄计算。

## API

```go
const DefaultMaxAge time.Duration = 30 * 24 * time.Hour

type Catalog struct { /* 私有字段 */ }

type SnapshotInfo struct {
    SchemaVersion int
    Sources       []SourceInfo
    VerifiedAt    time.Time
    ProviderCount int
    ModelCount    int
    SkippedModels int
}

type SourceInfo struct {
    ID            string
    URL           string
    SHA256        string
    ETag          string
    VerifiedAt    time.Time
    SkippedModels int
}

func Bundled() (*Catalog, error)
func Fetch(ctx context.Context, client *http.Client) (*Catalog, error)
func (c *Catalog) Info() SnapshotInfo
func (c *Catalog) Windows(provider string, at time.Time, maxAge time.Duration) (map[string]int, error)
func (c *Catalog) Guard(provider llm.Provider, maxAge time.Duration) (llm.Provider, error)
```

`Bundled` 在调用时解析嵌入的格式 2 快照，不联网、不读外部文件。旧格式 1、损坏数据或缺少任一必要来源都会报错。加载不检查当前日期。`Info` 返回副本，也复制 `Sources` 切片；nil Catalog 返回零值信息。

`Windows` 按数据源原始 provider ID 导出新 map，调用者修改它不会影响目录。模型名保持原样，不折叠大小写、不删除日期后缀、不猜别名、不跨 provider 借值，也不解释 `canonical_id`。

两源共用一个保守年龄：`VerifiedAt` 始终取两源时间的较早者。普通 provider 和 Codex 一起到期。`maxAge=0` 使用默认 30 天，正数可收紧或放宽，负数返回 `ErrInvalidArgument`。这项年龄策略不代表上游准确性保证。

| `Windows` 条件 | 结果 |
| --- | --- |
| provider 存在且未到期 | 新 map；未知或无效模型缺项 |
| provider 不存在，包括空字符串 | nil map，`ErrUnknownProvider` |
| `at - VerifiedAt >= maxAge` | nil map，`ErrStale` |
| nil/零值 Catalog、零时间、早于 VerifiedAt 或负年龄 | nil map，`ErrInvalidArgument` |

错误时不返回部分窗口。三个哨兵均可用 `errors.Is` 判断。时间/年龄校验先于到期检查，到期检查先于 provider 查询。

## 接入 Guard

先导表，再构造适配器，最后用**同一目录**的 `Guard` 包装。导表和包装使用同一年龄策略：

```go
catalog, err := modelmeta.Bundled()
if err != nil {
    return err
}
windows, err := catalog.Windows("openai", time.Now(), 0)
if err != nil {
    slog.Warn("model windows unavailable", "error", err)
    // 错误时 windows 已是 nil，底层 provider 将不声明目录窗口。
}
base := openai.NewProvider(apiKey, "gpt-4o",
    openai.WithModelContextWindows(windows))
provider, err := catalog.Guard(base, 0)
if err != nil {
    return err
}
a := agent.New(provider, registry, agent.WithContextWindowTokens(128000))
```

宿主必须把 `Guard` 返回的对象交给 `agent.New` 或 `agent.WithProviders`。不要同时注册底层对象，也不要在外面再套一层覆盖窗口查询的静态包装。裸 map 和适配器的副本不会自行到期：提前导表、到期后构造，以及现存 provider 长期运行，都依靠 Guard 在查询时返回未声明。

每次 `ContextWindow` 开始时采样系统时间。早于 `VerifiedAt` 或年龄达到上限时，返回 `(0, false)`；未到期才把原始模型字符串交给底层查询。空模型仍由适配器解释为构造默认模型。已经取得预算的轮次继续使用该轮预算，不中断正在执行的模型请求。时钟错误会影响真实年龄判断。

`Guard` 允许包装已经过期的目录，不在构造时返回 `ErrStale`。nil、typed-nil、没有窗口接口的 provider、无效目录或负年龄都会返回 nil provider 和 `ErrInvalidArgument`。包装失败后应处理错误，不要改用带旧数字的底层对象。

包装仅保证 `llm.Provider` 和 `llm.ContextWindowProvider` 两个接口。`Name`、`Chat`、`ChatStream` 原样委托，流式迭代器不预读；其他可选能力不会转发。包装只存取得时间、年龄和底层接口，不持有可写共享窗口表。底层 provider 的并发要求仍由底层实现决定。

建议底层只登记本目录导出的数字。混入手工登记后，Guard 会使整份声明一起到期。刷新得到新目录后，要重新导表、构造和接入新包装；不能用新目录的时间包旧窗口延长寿命。旧目录、旧 provider 和旧 Guard 都不会被 `Fetch` 修改。

内核有声明时用 `min(C, M)`，未声明时回到宿主构造窗口 C。C 默认 8,192 tokens；目录中的较大数字不会自动抬高 C。宿主需要按部署限制明确设置 `agent.WithContextWindowTokens`。未声明回退也不能代替宿主核验真实模型限制。

## 四个适配器

四包均有 `WithModelContextWindows(map[string]int)`，返回各包自己的 `ProviderOption`。所有接法最后都要调用同一 Catalog 的 Guard。

| 适配器 | 构造函数 | 数据源 provider ID |
| --- | --- | --- |
| `glm` | `NewProvider(apiKey, model string, opts ...ProviderOption) *Provider` | 中国默认端点用 `zhipuai`；海外端点由宿主明确选 `zai` |
| `openai` | `NewProvider(apiKey, model string, opts ...ProviderOption) *Provider` | 官方 API 用 `openai`；兼容服务按实际服务商选 |
| `openairesponses` | `NewProvider(apiKey, model string, opts ...ProviderOption) *Provider` | 官方 API 用 `openai` |
| `codex` | `NewProvider(model string, options ...ProviderOption) *Provider` | `openai-codex`；OAuth 仍由宿主给 codex 适配器配置 |

不能用 `Name()` 自动选来源。GLM 的名字为 `glm`，目录区分 `zhipuai` 和 `zai`；Responses 返回 `openai_responses`，来源却是 `openai`；Codex 返回 `codex`，来源是 `openai-codex`。兼容 API 的适配器名也不能说明实际服务商。

## 整批刷新与维护

`Fetch(ctx, client)` 顺序取得 v1 和 v2，两份都成功才返回新 Catalog。整个操作共用 10 秒 deadline，并尊重更短的调用者期限，不重试。每份正文实际读取上限 16 MiB，多读一个字节判断超限；只接受 HTTP 200，始终关闭响应体。client 为 nil 时使用内部 client，否则复制配置并禁用重定向及 Cookie Jar，不修改原对象。

任一步失败都返回 nil Catalog 和错误。不会发布 v1-only 的结果，不会把新 v1 与旧 v2 拼接，也不延长旧目录年龄。运行时只存内存，不探测 HOME、不读环境变量、不写缓存，也没有后台刷新。v2 的完整响应当日约 10.72 MiB，维护者需留意 16 MiB 上限的余量。

维护者在仓库根目录显式运行：

```sh
go run ./modelmeta/cmd/update-snapshot -output modelmeta/data/windows.json
```

目标文件必须显式指定。维护命令复用同一套下载、解码、裁剪和合并规则。验证通过后，将 source 按 ID 排序，并将所有层次的对象键按字典序序列化为紧凑 JSON，末尾保留一个换行。相同输入正文、ETag 和取得时间生成相同字节。命令写同目录临时文件，关闭后用 rename 替换目标；失败保留旧文件。不会自动挂到 build、test 或 `go generate`。

## 许可

窗口数据派生自 models.dev v1 和同仓库的实验 v2，按上游 MIT 许可分发。完整版权和许可文本位于 [LICENSE.models.dev](LICENSE.models.dev)，应原样保留。再次分发派生快照或包含嵌入数据的二进制时，也须随附该版权和许可文本。公开元数据许可与 Codex 模型服务的账号及服务条款分别适用。
