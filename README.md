# code-symbol-index-go

工作区源码的符号索引与检索库（附带 CLI）。支持增量更新、读写并发、落盘复用、
解析容错，所有输出与遍历顺序、并发度无关。

- 语言：Go（使用标准库 `go/parser` / `go/ast` 解析 Go 源码）
- 依赖：无第三方依赖

## 功能

1. **符号索引**：符号包含名称（Name）、种类（Kind）、位置（File/Line/Column），
   方法还记录接收者类型（Receiver）。
2. **增量更新**：文件新增 / 修改 / 删除后只重建受影响文件，更新结果与全量重建
   逐条一致，并可输出受影响文件清单。**调整影响索引结果的配置（`MaxFileSize`、
   `IgnorePatterns`）后，增量更新会自动重新评估全部文件，但报告中只列出索引结果
   实际发生变化的文件，结果与用新配置重新全量构建逐条一致。**
3. **并发检索**：索引以不可变快照（Snapshot）形式发布，更新期间读者看到的永远是
   某个已提交的完整版本；慢查询持有旧快照引用，不阻塞更新推进。
4. **落盘复用**：索引原子写入临时文件 → fsync → rename → fsync 目录，任何时刻
   崩溃都只会留下「上一个完整版本」或「新的完整版本」。
5. **解析容错与降级**：语法错误、非预期编码（非 UTF-8）、二进制内容、文件过大、
   权限不足、路径不可读都不会让整次索引失败。每个文件有 `ok` / `degraded` /
   `skipped` 三种状态，并带有**稳定的机器可读原因码（`reason_code`）**与人类可读
   原因；遍历阶段无法进入的目录记录在更新报告的 `issues` 列表中（含路径、范围和
   原因码），其余可读内容照常完成索引，最终结果与全量重建一致。
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

`index` 子命令在索引文件存在但检测到配置变化时，会全量重新评估并在 stderr 给出
`warning: indexing configuration changed; ...` 提示；遇到无法遍历 / 读取的路径时在
stderr 给出带路径和原因码的 warning（stdout 始终只输出报告 JSON）。索引文件损坏时会
自动从头重建并给出警告。

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
// report.ConfigChanged 为 true 时表示检测到影响结果的配置变化
// report.Issues 列出无法遍历/读取但被跳过的路径（含原因码）

// 调整影响索引结果的配置后直接再次 Update：
cfg.MaxFileSize = 10 << 20
if err := idx.SetConfig(cfg); err != nil { /* 配置非法 */ }
report, err = idx.Update(context.Background())
// -> 全量重新评估，但 Affected 只包含结果真正变化的文件；
//    新快照与用新配置 Rebuild 的结果逐条一致。

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

#### 配置变更对增量更新的影响

`MaxFileSize` 和 `IgnorePatterns` 属于**影响索引结果**的配置（`Root`、`Concurrency`
不影响结果，不参与判定）。每个已提交快照都记录了生成它所用配置的签名
（`Snapshot.SettingsSig`，由 `Config.Signature()` 对上限和去空白、排序后的忽略模式
取哈希得到）。

增量更新开始时：

1. 计算当前配置签名并与当前快照的签名比较；
2. **签名不同**（含从旧版本落盘文件加载、尚无签名字段的情况）时，对遍历到的所有
   文件重新处理一遍（这是配置调整后的一次性代价，配置不常改，可接受）；
3. 逐个比较新旧文件结果（状态、原因码、符号集合；不含 size/mtime 等易变元数据）：
   - 结果不同 → 计入 `Modified`（如 `skipped/too_large` → `ok`，或 `ok` →
     `skipped/too_large`）；
   - 新配置下才出现的文件（例如取消忽略）→ 计入 `Added`；
   - 新配置下消失的文件（例如新增忽略、删除）→ 计入 `Removed`；
   - 结果没变的文件**不**出现在受影响列表中；
4. 报告 `ConfigChanged = true`；随后再次更新即恢复为普通增量（配置未再变时受影响
   列表为空）。

因此：

- 把 `MaxFileSize` **调大**：之前因超限被跳过的文件会被重新解析，出现在
  `Modified` 中，符号可查到；
- 把 `MaxFileSize` **调小**：之前已建好、现在超限的文件会重新按新上限处理为
  `skipped/too_large`，其符号从查询结果消失，并出现在 `Modified` 中；
- 增加 / 删除忽略模式：相应文件出现在 `Removed` / `Added` 中；
- 无论怎么改配置，增量更新后的快照与用新配置执行一次 `Rebuild` 的结果**逐条一致**。

从旧版本持久化文件打开的索引（没有签名）会在第一次 `Update` 时自动重处理一次，
结果不变的文件不会计入受影响列表，之后即恢复普通增量。

### 全量重建

`Rebuild` 强制忽略旧快照重新索引；`Update` 在没有旧快照时自动执行全量重建。
全量重建同样会跳过不可读路径并在报告中给出 `issues`。

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
| `Root` | 工作区根目录 | 不能为空；不参与配置签名 |
| `Concurrency` | 解析 worker 数 | 必须 >= 1，否则 `New`/`SetConfig` 返回错误；不参与配置签名 |
| `IgnorePatterns` | 忽略 glob 列表 | 见下；参与配置签名 |
| `MaxFileSize` | 单文件字节上限 | >= 0；0 表示不限制；参与配置签名 |

运行时可通过 `idx.SetConfig(cfg)` 替换配置（会先 `Validate`，非法配置被拒绝且保留
旧配置）；下一次 `Update` 自动检测结果相关配置是否变化，变化时按「配置变更对增量
更新的影响」一节处理。

### 忽略规则

- 不含路径分隔符的模式（如 `vendor`、`.git`、`*.gen.go`）匹配任意路径组件；
- 含分隔符的模式（如 `foo/bar.go`）匹配完整相对路径；
- 使用 `filepath.Match` 语法（`*`、`?`、`[...]`）。
- 命中忽略规则的目录会被直接跳过（不进入遍历）。

### 超出边界的行为

- 文件大小超过 `MaxFileSize`：状态 `skipped`，原因码 `too_large`，原因
  `file too large: N bytes exceeds limit M`；超限文件不会被读取，大文件不会拖垮扫描；
- 二进制内容（即使以 `.go` 结尾）：状态 `skipped`，原因码 `binary`（见下）；
- 非 `.go` 后缀的文件（可执行文件、压缩包、图片等）根本不会进入索引，连跳过记录
  都不产生；
- 并发度配置为 0：`New` / `SetConfig` 返回错误，不会建立 / 替换索引配置；
- 根目录不存在或根本无法打开：更新返回遍历错误，不会发布新快照（仅此情况算整次
  更新失败）。

## 跳过与降级的原因分类

每个 `FileResult` 除自由文本 `reason` 外，还带有稳定的原因码 `reason_code`
（字段 `FileResult.ReasonCode`，JSON 键 `reason_code`），可用于程序化区分：

| 状态 | reason_code | 触发情况 | 是否仍有部分符号 |
| --- | --- | --- | --- |
| `degraded` | `parse_error` | Go 语法错误 | 是，尽力提取 |
| `skipped` | `too_large` | 大小超过 `MaxFileSize`（不读取内容） | 否 |
| `skipped` | `binary` | 前 8 KiB 内出现 NUL 字节（git 同款启发式），按二进制处理 | 否 |
| `skipped` | `invalid_utf8` | 字节不是合法 UTF-8 | 否 |
| `skipped` | `permission_denied` | stat / 读取被系统拒绝（`EACCES`/`EPERM`） | 否 |
| `skipped` | `not_readable` | 其他 I/O 原因（悬空符号链接、I/O 错误等） | 否 |
| `ok` | （空） | 正常解析 | 全部 |

二进制判定与大小上限的先后顺序：**先比较大小**（超限直接 `too_large`，完全不读），
读取后**先查 NUL 字节**（`binary`），再查 UTF-8（`invalid_utf8`），最后才交给
Go 解析器。因此：

- 多 MiB 的大二进制：命中大小上限，不产生读取 / 扫描开销；
- 小的伪装二进制：最多扫描前 8 KiB，开销有界，不会污染符号表，也不会被记成解析错误；
- 判定结果稳定：同一份内容在任何并发度、任何遍历顺序下原因码一致。

### 不可读路径的降级（issues）

文件在遍历阶段都能枚举，但读取 / stat 发生在处理阶段，因此单个不可读**文件**表现为
快照中的一条 `skipped` 记录（原因码 `permission_denied` 或 `not_readable`），同目录
其他文件照常索引；权限恢复后，下一次增量更新会自动把它补回来（新增为已索引）。

无法进入的**目录**（如没有 `x` 权限）在遍历时就会失败，这类路径记录在更新报告的
`UpdateReport.Issues` 中：

| 字段 | 含义 |
| --- | --- |
| `Path` | 出问题的路径（相对工作区根；根本身为 `.`） |
| `Scope` | `dir`（无法遍历的目录，整棵子树被跳过）或 `file`（无法 stat 的文件项） |
| `ReasonCode` | `permission_denied` 或 `not_readable` |
| `Reason` | 包含底层错误的可读描述 |

`issues` 按路径排序；一次更新中任意数量的坏路径都只会产生对应的 issue / skip 记录，
**不会让整次更新失败**，其余可读内容正常完成索引，最终快照与全量重建一致。目录
恢复可访问后，其下文件会在下次增量更新中作为 `Added` 补入，对应 issue 消失。
唯一的硬失败是工作区根目录本身无法打开（如不存在），此时不发布新快照。

## 持久化与重启恢复

- 落盘：临时文件（同目录 `.index.json.tmp`）→ fsync → `rename` → fsync 目录。
  `rename` 在同文件系统上是原子的，因此崩溃后要么是旧完整文件，要么是新完整文件。
- 加载：读取并校验格式版本与结构；解码失败返回 `ErrCorrupt`（损坏），文件不存在
  返回 `os.IsNotExist` 错误（`Open` 会得到空索引）。
- 加载后重建派生符号表，内存中不会残留半成品状态。
- 配置签名（`settings_sig`）是新增的可选 JSON 字段：旧版本写出的索引没有该字段，
  新版本可以直接读取；第一次 `Update` 时检测到签名缺失会按「配置变更」重新评估一次
  后把签名写回，不需要手工迁移，格式版本号保持不变。
- 建议：重启后先 `Open` 再 `Update`，即可在复用旧索引的同时增量追上最新内容；
  如果启动参数（如 `--max-size`、`--ignore`）与上次建索引时不同，`Update` 会自动
  协调到新配置，结果与全量重建一致。

## 并发模型

- `Index` 内部用 `atomic.Pointer[Snapshot]` 持有当前快照。
- 读操作（`Snapshot` / `Search` / `Outline`）无锁、不可变；慢查询即使长期持有旧
  快照引用，更新仍可继续推进，且旧快照始终可查、内部一致。
- 更新在后台构建新快照，完成后一次性原子切换；切换前失败不会产生部分状态（既不会
  出现文件级混合——一半文件来自新配置、一半来自旧配置；也不会出现字段级混合——
  同一文件的符号和状态来自两次不同处理）。
- 更新全程持有配置读锁，`SetConfig` 取写锁；配置不会在一次更新进行到一半时被换掉，
  所以已发布快照中的 `Files` 与它记录的 `SettingsSig` 始终是同一套配置的产物。

## 构建产物与版本库卫生

`go build` 产生的二进制不提交到版本库：仓库根目录的 `.gitignore` 忽略
`cmd/codeidx/codeidx`、`codeidx`、`*.exe`、`*.test`、覆盖率文件和本地 `.codeidx/`
目录。若历史上误提交过二进制，用 `git rm --cached <path>` 将其移出索引（保留或删除
工作区文件均可）后提交即可。

## 测试与验证

```bash
go test ./... -race -count=1
```

覆盖：全量构建与查询、不同并发度确定性、增量更新与全量重建一致性、touch 不误判、
并发读写（race）、慢查询不阻塞更新、落盘与损坏恢复、语法错误 / 坏编码 / 超大文件
容错、忽略规则、分页边界、配置校验、CLI 端到端，以及本次新增的：

- **配置变更前后的增量 / 全量一致性**
  （`config_change_test.go`）：
  - 调大 / 调小 `MaxFileSize` 后，受影响文件完整准确地出现在 `Modified` 中，
    其余文件不在列表里；符号的出现 / 消失正确；随后的空更新报告为空；
  - 增加 / 移除忽略模式，文件分别出现在 `Removed` / `Added`；
  - 配置变更与文件内容变更同时发生时两者都被捕获；
  - 配置签名对模式顺序 / 空白不敏感、对上限 / 模式增减敏感、与 `Root` /
    `Concurrency` 无关；
  - 跨持久化（`Save` → `Open` → `Update`）协调一致，且只协调一次；
  - 旧格式快照（无签名字段）首次更新重处理一次、结果不变的文件不进受影响列表；
  每个用例最终都对增量快照和「新配置下全新索引 + `Rebuild`」的快照做逐文件、
  逐符号比对。
- **不可读路径的跳过与原因区分**（`unreadable_test.go`）：
  - 0000 权限的目录：更新不中断，`issues` 中恰有一条 `scope=dir`、
    `reason_code=permission_denied`、路径精确的记录；其他目录正常索引；恢复权限后
    子树文件增量补入、issue 消失；
  - 0000 权限的文件：快照中为 `skipped/permission_denied`，兄弟文件不受影响，
    恢复权限后增量补回；
  - 悬空符号链接：`skipped/not_readable`；
  - 根目录不存在仍然硬失败且不发布新快照（保留原有契约）。
  - 注意：权限用例在 root 或 DAC 被绕过的环境下会自动跳过（测试内置
    `permissionEnforced` 探测）；在普通用户下可验证真实内核行为，例如：
    `su nobody -s /bin/sh -c 'TMPDIR=<可写目录> go test . -run TestUnreadable -v'`。
- **二进制与超大文件处理**（`binary_test.go`）：
  - 伪装成 `.go` 的 ELF 二进制 → `skipped/binary`，不产生解析错误、不产生符号；
  - 大小判定先于二进制判定，超大文件不被读取（多 MiB 二进制开销有界）；
  - NUL 仅在前 8 KiB 嗅探窗口内判定（边界稳定），合法 UTF-8 多字节文本不误判；
  - 非 `.go` 的二进制 / 压缩包 / 文本根本不进索引；
  - 原因码取值稳定性（防止重命名破坏 API 契约）。

跨平台 / 跨权限环境运行：

```bash
go test ./... -race -count=1          # 常规：权限用例在 root 下自动跳过
go vet ./... && gofmt -l .            # 静态检查与格式（gofmt 应无输出）
go build -o /tmp/codeidx ./cmd/codeidx   # 确认 CLI 可构建且不产出仓库内二进制
```

