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
5. **解析容错**：语法错误、非预期编码（非 UTF-8）、文件过大都不会让整次索引失败；
   每个文件有 `ok` / `degraded` / `skipped` 三种状态，原因字段可区分，错误链可定位
   到具体文件。
6. **检索**：支持按名称（精确）、前缀、种类、所属文件查询；支持单文件大纲；
   排序稳定，分页边界确定。
7. **可配置**：并发度、忽略规则、单文件大小上限均可配置；超出边界行为确定。

## 安装

```bash
go build -o codeidx ./cmd/codeidx
```

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
排序并集），且新快照与全量重建结果完全一致。

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

- 文件大小超过 `MaxFileSize`：状态 `skipped`，原因 `file too large: N bytes exceeds limit M`；
- 并发度配置为 0：`New` 返回错误，不会建立索引；
- 根目录不存在：更新返回遍历错误，不会发布新快照。

## 容错

| 情况 | 状态 | 原因示例 |
| --- | --- | --- |
| 语法错误 | `degraded` | `parse error: ...`（仍提取部分符号） |
| 非 UTF-8 编码 | `skipped` | `invalid UTF-8 encoding` |
| 文件过大 | `skipped` | `file too large: ...` |
| 读取失败 | `skipped` | `read error: ...` / `stat error: ...` |

单个文件的任何失败都不会中断整次索引；错误信息包含文件路径，可定位到具体文件。

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
并发读写（race）、慢查询不阻塞更新、落盘与损坏恢复、语法错误 / 坏编码 / 超大文件
容错、忽略规则、分页边界、配置校验、CLI 端到端。
