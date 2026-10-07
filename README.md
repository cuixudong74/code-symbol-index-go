# code-symbol-index-go

工作区源码的符号索引与检索库（附带 CLI）。支持增量更新、读写并发、落盘复用、
解析容错，所有输出与遍历顺序、并发度无关。

- 语言：Go（使用标准库 `go/parser` / `go/ast` 解析 Go 源码）
- 依赖：无第三方依赖

## 功能

1. **符号索引**：符号包含名称（Name）、种类（Kind）、位置（File/Line/Column），
   方法还记录接收者类型（Receiver）。
2. **增量更新**：文件新增 / 修改 / 删除后只重建受影响文件，更新结果与全量重建
   逐条一致，并可输出受影响文件清单。
3. **并发检索**：索引以不可变快照（Snapshot）形式发布，更新期间读者看到的永远是
   某个已提交的完整版本；慢查询持有旧快照引用，不阻塞更新推进。
4. **落盘复用**：索引原子写入临时文件 → fsync → rename → fsync 目录，任何时刻
   崩溃都只会留下「上一个完整版本」或「新的完整版本」。
5. **解析容错**：语法错误、非预期编码（非 UTF-8）、二进制文件、文件过大、
   路径不可读都不会让整次索引失败；每个文件有 `ok` / `degraded` / `skipped`
   三种状态，并配有**机器可读的原因码（ReasonCode）**，错误信息可定位到具体路径；
   不可读的目录/文件单独记录在快照的 `Skipped` 表中，其余可读内容正常完成索引。
6. **检索**：支持按名称（精确）、前缀、种类、所属文件查询；支持单文件大纲；
   排序稳定，分页边界确定。
7. **可配置**：并发度、忽略规则、单文件大小上限均可配置；超出边界行为确定。

## 安装

```bash
go build -o codeidx ./cmd/codeidx
```

编译产物属于本地构建结果，**不要提交进版本库**：仓库根目录的 `.gitignore`
已忽略 `codeidx`、`cmd/codeidx/codeidx`、`*.test` 等构建产物以及 `/.codeidx/`。
可用 `git check-ignore <path>` 验证某路径是否被忽略。

## CLI

```bash
# 建立（或增量更新）索引，默认写入 <root>/.codeidx/index.json
codeidx index --root .

# 精确名称
codeidx query --root . --name Person

# 前缀 + 种类 + 文件过滤
codeidx query --root . --prefix Get --kind function
codeidx query --root . --file src/a.go --limit 20 --offset 10

# 单文件大纲
codeidx outline --root . --file src/a.go

# 索引状态
codeidx status --root .
```

`index` 子命令在索引文件损坏时会自动从头重建并给出警告。

### 配置项（CLI flags）

| flag | 说明 | 默认值 |
| --- | --- | --- |
| `--root` | 工作区根目录 | `.` |
| `--index` | 索引文件路径 | `<root>/.codeidx/index.json` |
| `--concurrency` | 索引 worker 数 | `runtime.NumCPU()` |
| `--max-size` | 单文件字节上限，0 表示用默认值 | `5242880`（5 MiB） |
| `--ignore` | 忽略 glob 模式，可重复 | `.git`、`.codeidx` |

## 作为库使用

```go
import (
    "context"
    index "github.com/cuixudong74/code-symbol-index-go"
)

cfg := index.DefaultConfig("/path/to/workspace")
cfg.Concurrency = 4
cfg.MaxFileSize = 2 << 20
cfg.IgnorePatterns = []string{".git", "vendor", "*.gen.go"}

idx, err := index.New(cfg)            // 空索引
// 或从磁盘打开（不存在则为空索引，损坏返回 index.ErrCorrupt）：
// idx, err := index.Open(cfg, "/path/to/.codeidx/index.json")

report, err := idx.Update(context.Background()) // 增量更新；首次等同全量
// report.Added / Modified / Removed / Affected

snap := idx.Snapshot()                // 当前已提交版本，不可变、可并发查询
results := snap.Search(index.Query{Prefix: "Get", Kind: index.KindFunction, Limit: 20})
outline := snap.Outline("src/a.go")

idx.Save("/path/to/.codeidx/index.json") // 原子落盘
```

## 符号种类

| Kind | 说明 |
| --- | --- |
| `package` | 包名 |
| `function` | 函数 |
| `method` | 方法（含 Receiver） |
| `type` | 类型定义 |
| `variable` | 包级变量 / 局部变量 |
| `constant` | 常量 |
| `field` | 结构体字段 / 接口方法 |

## 索引语义

### 确定性

- 文件遍历结果最终按相对路径排序；符号全局按
  `(File, Line, Column, Name, Kind)` 稳定排序。
- 并发解析时每个文件的结果写入按遍历顺序预留的槽位，合并后统一排序。
- 因此同一份内容在任何遍历顺序、任何并发度（1/2/4/8/16…）下，检索结果逐条一致。

### 增量更新

1. 遍历工作区收集所有 `.go` 文件（应用忽略规则）。
2. 与上一快照逐文件比对：
   - 旧快照中不存在 → **新增（added）**；
   - 旧快照中存在但指纹变化 → **修改（modified）**；
   - 旧快照中存在但遍历不到 → **删除（removed）**。
3. 仅对新增 / 修改的文件重新解析；未变化文件复用上一快照的结果。
4. 新快照在内存中构建完成后，通过原子指针切换发布；在此之前读者看到的始终是旧版本。

指纹（Fingerprint）：

- 可读取的文件使用内容的 SHA-256；
- 因过大而未读取的文件使用 `stat:<size>:<mtime>`。
- 判定修改时先比较 size/mtime，不一致再哈希内容确认（`touch` 改了 mtime 但内容不变
  不会触发重建）。

更新完成后会输出 `Added` / `Modified` / `Removed` / `Affected`（Affected 为三者的
排序并集），且新快照与全量重建结果完全一致（逐条文件的状态、原因码与符号都一致）。

#### 配置变更对增量更新的影响

调整会影响索引结果的配置后，不需要手动全量重建；下一次 `Update` 会检测配置差异并把
**结果会发生变化的那些文件**精确地列入受影响清单，重新处理后与「在新配置下全量重建」
逐条一致。报告中 `ConfigChanged=true` 表示本次增量检测到配置/规则变化。

快照会记录产生它时的 `PolicyVersion`（内建规则版本）与 `MaxFileSize`（大小上限），
因此**即使经过落盘重启**也能识别配置变化：

| 变化 | 受影响文件 | 说明 |
| --- | --- | --- |
| `MaxFileSize` 调大 | 原先因超限被跳过、现在在上限内的文件 | 从 `skipped/too_large` 变为正常索引 |
| `MaxFileSize` 调小 | 原先已索引、现在超过新上限的文件 | 变为 `skipped/too_large`，符号消失 |
| `MaxFileSize` 在 0（不限制）与有限值间切换 | 大小跨越上限的文件 | 同上 |
| 内建规则升级（`CurrentPolicyVersion`） | 几乎所有未变文件（升级当次一次性重处理） | 只有「新旧规则下都判定 too_large」的文件被跳过以免读取超大文件 |
| `IgnorePatterns` 变化 | 由遍历结果自然覆盖 | 新被忽略的文件出现在 `Removed`；取消忽略的出现在 `Added` |

与大小无关的规则（如二进制识别）只依赖规则版本。规则升级后只在**升级后的第一次**
更新付出一次全量重处理开销（仍走增量通道、原子发布）；新快照打上新版号之后，后续
更新回到「只看内容变化」的快速路径。仅调大/调小上限时，开销与真正受影响的文件数
成正比（超限文件只做 `stat`，不读取内容）。

### 全量重建

`Rebuild` 强制忽略旧快照重新索引；`Update` 在没有旧快照时自动执行全量重建。

## 查询语义

- `Query.Name`：精确匹配名称（区分大小写）。
- `Query.Prefix`：匹配名称前缀（区分大小写）。
- `Query.Kind`：匹配种类。
- `Query.File`：匹配文件（相对路径）。
- 多个条件之间为 **与** 关系；零值条件表示不限制。
- 结果始终按 `(File, Line, Column, Name, Kind)` 排序。

### 分页与截断（边界行为）

- 先过滤，再应用 `Offset`，最后应用 `Limit`。
- `Offset`：跳过前 N 条匹配；`Offset >= 匹配总数` 时返回空切片。
- `Limit <= 0`：不限制条数（默认）。
- 分页遍历：以固定页大小递增 Offset，直到某页为空，可无遗漏地取完全部结果。
- 多次同一查询返回顺序稳定一致。

## 配置项

| 字段 | 说明 | 约束 |
| --- | --- | --- |
| `Root` | 工作区根目录 | 不能为空 |
| `Concurrency` | 解析 worker 数 | 必须 >= 1，否则 `New` 返回错误 |
| `IgnorePatterns` | 忽略 glob 列表 | 见下 |
| `MaxFileSize` | 单文件字节上限 | >= 0；0 表示不限制 |

### 忽略规则

- 不含路径分隔符的模式（如 `vendor`、`.git`、`*.gen.go`）匹配任意路径组件；
- 含分隔符的模式（如 `foo/bar.go`）匹配完整相对路径；
- 使用 `filepath.Match` 语法（`*`、`?`、`[...]`）。
- 命中忽略规则的目录会被直接跳过（不进入遍历）。

### 超出边界的行为

- 文件大小超过 `MaxFileSize`：状态 `skipped`，原因码 `too_large`，
  原因 `file too large: N bytes exceeds limit M`（不读取文件内容）；
- 内容含 NUL 字节（二进制）：状态 `skipped`，原因码 `binary`；
- 并发度配置为 0：`New` 返回错误，不会建立索引；
- 根目录不存在/不是目录/无法打开：更新返回遍历错误，不会发布新快照；
- 工作区内部个别文件/目录不可读：跳过并记录原因码 `access_denied` 与具体路径，
  其余内容正常索引，不返回错误。

## 容错

### 文件级结果（`FileResult.Status` / `ReasonCode`）

| 情况 | 状态 | 原因码 `ReasonCode` | 原因示例 |
| --- | --- | --- | --- |
| 语法错误（仍提取部分符号） | `degraded` | `parse_error` | `parse error: ...` |
| 二进制文件（内容含 NUL 字节） | `skipped` | `binary` | `binary file: NUL byte detected; ...` |
| 非 UTF-8 编码 | `skipped` | `invalid_encoding` | `invalid UTF-8 encoding` |
| 文件大小超过 `MaxFileSize`（不读内容） | `skipped` | `too_large` | `file too large: N bytes exceeds limit M` |
| stat/读取失败（含权限不足） | `skipped` | `access_denied` | `read error: open ...: permission denied` |

原因码是稳定的机器可读标识，程序应基于 `ReasonCode` 做分支；`Reason` 是包含底层错误的
人类可读文本。二进制检测在 UTF-8 校验之前：同时是二进制又非 UTF-8 的文件记为更具体的
`binary`。判定顺序为：stat → 大小上限（超限即止）→ 读内容 → 二进制 → UTF-8 → 解析。

### 路径级跳过（`Snapshot.Skipped` / `UpdateReport.SkippedPaths`）

目录无法读取（如权限 000）或条目无法 stat 时，**整次更新不会失败**：

- 该路径记录到 `Snapshot.Skipped`（键为相对路径）和本次报告的 `SkippedPaths`，带有
  - `Path`：可定位的相对路径；
  - `IsDir`：区分「目录读不了」（其下内容全部缺席）与「文件读不了」；
  - `ReasonCode`：统一为 `access_denied`；
  - `Reason`：底层错误（permission denied 等）。
- 无法遍历的目录下的旧索引条目按 `Removed` 处理，因此结果与新配置下全量重建一致。
- 其余可读路径照常完成索引与查询。
- 边界：**根目录本身**无法打开或不是目录时仍返回错误（此时没有任何可索引内容），
  与「工作区里个别路径读不了」相区分。

CLI `index` 会把每个跳过路径以 `warning:` 打到 stderr（stdout 仍是 JSON 报告），
`status` 输出 `paths_unreadable` 与 `skipped_paths` 明细。

### 二进制与超大文件的性能

- 只有扩展名 `.go` 的文件才进入候选集合；其他名字的大文件（`blob.bin`、编译产物、
  镜像等）遍历时连读取都不会发生。
- 超大 `.go` 文件只做 `stat` 即跳过，指纹使用 `stat:<size>:<mtime>`，不读取内容；
  含 NUL 字节的伪 `.go` 二进制在读取后立即判定，不进入解析。
- 因此工作区中存在大量/超大二进制文件时，扫描与查询开销保持在「目录遍历」级别。

## 持久化与重启恢复

- 落盘：临时文件（同目录 `.index.json.tmp`）→ fsync → `rename` → fsync 目录。
  `rename` 在同文件系统上是原子的，因此崩溃后要么是旧完整文件，要么是新完整文件。
- 加载：读取并校验格式版本与结构；解码失败返回 `ErrCorrupt`（损坏），文件不存在
  返回 `os.IsNotExist` 错误（`Open` 会得到空索引）。
- 加载后重建派生符号表，内存中不会残留半成品状态。
- 建议：重启后先 `Open` 再 `Update`，即可在复用旧索引的同时增量追上最新内容。

## 并发模型

- `Index` 内部用 `atomic.Pointer[Snapshot]` 持有当前快照。
- 读操作（`Snapshot` / `Search` / `Outline`）无锁、不可变；慢查询即使长期持有旧
  快照引用，更新仍可继续推进，且旧快照始终可查、内部一致。
- 更新在后台构建新快照，完成后一次性原子切换；切换前失败不会产生部分状态。

## 测试

```bash
go test ./... -race -count=1
```

覆盖：全量构建与查询、不同并发度确定性、增量更新与全量重建一致性、touch 不误判、
并发读写（race）、慢查询不阻塞更新、落盘与损坏恢复、语法错误 / 坏编码 / 二进制 /
超大文件容错、忽略规则、分页边界、配置校验、CLI 端到端。

与本次改动直接相关的用例：

- `TestSizeLimitChangeIncrementalMatchesRebuild` / `TestSizeLimitChangeAcrossRestart`：
  大小上限调大、调小（含落盘重启）后，受影响文件清单完整准确，且逐条与新配置下的
  全量重建一致；再次更新为 no-op。
- `TestPolicyUpgradeReclassifiesBinary`：内建规则版本升级后，旧状态被一次性纠正，
  结果与全量重建一致，之后回到快速路径。
- `TestUnreadablePathsDoNotFailUpdate`：不可读文件/目录不中断更新；可读文件正常
  索引；不可读目录下内容缺席并记为 `Removed`；原因码 `access_denied` 且能定位路径；
  与全量重建一致。root 环境下会自动降权（`runuser`/`setpriv` → `nobody`）在子进程
  中验证真实权限错误；无法降权时跳过。
- `TestMissingRootStillErrors`：根目录不可用仍返回错误（边界行为不变）。
- `TestBinaryGoFileSkipped` / `TestNonGoBinaryAndLargeFilesAreCheap` /
  `TestDistinctReasonCodes`：`.go` 命名的二进制以 `binary` 稳定跳过；非 `.go` 大文件
  不被读取；超大 `.go` 文件只 stat 不读内容；各原因码可区分。
- `TestCLIBinarySkipAndSizeLimitChange`：CLI 端到端验证上限调整的增量补索引与
  `status` 的跳过统计。

### 手工验证

```bash
go build -o /tmp/codeidx ./cmd/codeidx
# 1) 小上限下 big.go 被跳过，blob.go(二进制) 被跳过
/tmp/codeidx index --root <ws> --max-size 100
# 2) 调大到默认上限：ConfigChanged=true，Modified 精确列出 big.go
/tmp/codeidx index --root <ws>
# 3) status 查看 files_skipped / paths_unreadable / policy_version
/tmp/codeidx status --root <ws>
```
