# code-symbol-index-go

工作区源码符号索引与检索库。它支持 Go 源文件的容错解析、确定性结果、增量更新、读写并发、原子落盘和重启复用。

## 快速使用

```go
package main

import (
    "context"
    "fmt"

    symbolindex "github.com/cuixudong74/code-symbol-index-go"
)

func main() {
    cfg := symbolindex.DefaultConfig("/path/to/workspace")
    cfg.IgnorePatterns = []string{"vendor/", "*.gen.go"}

    store, err := symbolindex.NewStore("/path/to/index-store")
    if err != nil { panic(err) }

    engine, err := symbolindex.NewEngineWithStore(cfg, store)
    if err != nil { panic(err) }
    defer engine.Close()

    report, err := engine.UpdateAndSave(context.Background())
    if err != nil { panic(err) }
    fmt.Printf("version=%d affected=%+v\n", report.Version, report.Affected)

    symbols, _ := engine.Search(symbolindex.Query{Prefix: "Get", Kind: symbolindex.KindFunction, Limit: 50})
    fmt.Println(symbols)
}
```

## 支持的符号

内置解析器扫描 `*.go`，提取包级声明：

| 种类 | Go 声明 |
| --- | --- |
| `function` | 包级 `func Name(...)` |
| `method` | 带 receiver 的 `func (r T) Name(...)` |
| `type` | `type Name ...` |
| `constant` | `const` / `const (...)` 中的值名 |
| `variable` | `var` / `var (...)` 中的值名 |

`Symbol` 包含：

- `Name`：符号名；
- `Kind`：上述种类；
- `Path`：相对工作区根的 `/` 分隔路径；
- `Line`、`Column`：Go token 位置，行号从 1 开始，列按字节计；
- `StartOffset`、`EndOffset`：文件内字节偏移，`EndOffset` 为排除式结束位置。

局部变量、结构体字段和导入名不是包级符号，不进入索引。

## 确定性语义

以下操作不依赖文件系统返回顺序或 Go map 遍历顺序：

1. 扫描结果按相对路径字典序排序；
2. 文件可以按配置的并发度读取和解析；
3. 记录按扫描顺序写入，构建快照前再按路径排序；
4. 全局符号按以下键排序：
   `Path`、`Line`、`Column`、`StartOffset`、`Kind`、`Name`；
5. 每个文件的大纲按同一顺序排列，通常表现为源码声明顺序；
6. 受影响文件列表按路径字典序输出。

因此，同一份工作区内容无论使用 `Workers=1` 还是更高并发度，也无论目录枚举顺序如何，提交后的文件记录与符号结果逐条一致。测试 `TestBuilderConcurrencyDoesNotAffectResult` 和增量全量对比测试锁定该语义。

## 增量更新

`Update()` 的判定流程：

1. 重新扫描当前配置可见的普通 `*.go` 文件；
2. 新路径加入 `Affected.Added`；
3. 扫描不到的旧路径加入 `Affected.Deleted`；
4. 对仍存在的文件比较大小和修改时间：
   - 完全相同且旧状态不是 `skipped_too_large` 时直接复用旧记录，不读取内容；
   - 元数据变化时读取文件并计算 SHA-256；
   - 哈希、状态或错误码变化时进入 `Affected.Modified`；
   - 仅元数据变化但内容与可比较状态不变时进入 `Affected.MetadataChanged`；
5. 所有记录处理完成后一次性发布一个新 `Snapshot`。

`Report.Affected` 明确给出受影响文件。更新后的索引语义与重新创建引擎执行全量构建相同；项目测试会比较每个文件记录、哈希、状态、原因以及全局符号顺序。

修改时间用于避免不必要读取，SHA-256 用于判断内容是否真实变化。若文件的修改时间变化但内容哈希相同，不会生成不同符号结果。

## 并发与版本一致性

`Snapshot` 是不可变对象。引擎通过原子指针发布：

- `Search()` 和 `Outline()` 不获取更新锁，只读取调用开始时取得的快照；
- 慢查询可以长时间持有旧快照，不会阻止新更新构建和发布；
- 每次查询看到的是某个已提交完整版本，不会看到部分文件或字段；
- 同一个快照内不会混合旧版和新版文件；
- `Update()` 串行化以维护单调版本号，更新期间查询仍读旧版本。

搜索过程中如果另一个更新提交，当前调用继续使用旧快照；下一次调用才使用新版本。

## 查询条件、排序和分页

`Query` 支持：

- `Name`：精确名称；同时提供 `Name` 和 `Prefix` 时，精确名称优先；
- `Prefix`：名称前缀匹配；
- `Kind`：按符号种类过滤；
- `Path`：限定所属文件；
- `Offset`、`Limit`：分页。

分页边界是稳定且确定的：

- `Limit <= 0` 使用 `Config.DefaultPageSize`，默认 100；
- `Limit > Config.MaxQueryLimit` 被截断到该上限，默认上限 1000；
- `Offset < 0` 返回错误；
- `0 <= Offset < Total` 返回对应页；
- `Offset >= Total` 返回空符号切片，但 `Total` 仍为过滤后的完整总数；
- `HasMore` 只表示当前结束位置之后是否还有结果；
- 搜索返回的符号顺序固定为全局符号顺序，不会按名称二次排序。

`Outline(path)` 返回单个文件的完整大纲，不做分页。文件存在但解析失败时也会返回记录及状态；不存在则第二个返回值为 `false`。

## 容错策略

单个文件异常不会让整个索引失败。文件状态包括：

| 状态 | reasonCode | 策略 |
| --- | --- | --- |
| `ok` | 空 | 正常索引 |
| `skipped_too_large` | `file_too_large` | 文件大小严格大于 `MaxFileSize` 时不读取；等于上限允许 |
| `skipped_invalid_utf8` | `invalid_utf8` | 内容不是合法 UTF-8，跳过解析 |
| `parse_error` | `syntax_error` | 标记文件；若 Go parser 返回局部 AST，则保留可恢复声明 |
| `read_error` | `read_failed` | 读取失败，保留文件记录和错误原因 |

检查顺序为文件类型/忽略规则、大小限制、读取、UTF-8、语法解析。因此一个同时过大且编码非法的文件会报告为 `file_too_large`。

每次 `Report` 都包含：

- `IndexedFiles`：正常文件数；
- `SkippedFiles`：按策略跳过的文件数；
- `FailureFiles`：读取、编码或解析失败文件数；
- `Errors`：结构化 `FileError` 链路，包含具体相对路径、操作/状态和错误信息。

普通文件之外的符号链接、套接字、设备文件等确定性忽略，不作为错误。非 Go 文件不生成文件记录。

## 忽略规则

`IgnorePatterns` 使用工作区相对的 gitignore 风格规则：

- 没有 `/` 的模式匹配任意深度的路径组件，例如 `generated` 可匹配 `a/generated/b.go`；
- 含 `/` 的模式锚定工作区根，例如 `vendor/` 只裁剪根目录下的 `vendor`；
- 结尾 `/` 表示只匹配目录；目录命中后直接剪枝，其后代不会再被重新包含；
- `*` 不跨 `/`，`?` 匹配一个非 `/` 字符，`**` 可跨目录；
- 以 `!` 开头表示重新包含，但不能重新包含已剪枝目录内的后代；
- 空行和 `#` 注释被忽略。

## 配置

```go
type Config struct {
    Root            string
    Workers         int
    MaxFileSize     int64
    IgnorePatterns  []string
    DefaultPageSize int
    MaxQueryLimit   int
}
```

- `Root`：必填，规范化为绝对路径；
- `Workers`：有界读取/解析并发度；`<=0` 使用 `runtime.NumCPU()`；
- `MaxFileSize`：字节上限；0 使用默认 10 MiB，负数禁用大小限制；
- `DefaultPageSize`：默认 100；
- `MaxQueryLimit`：默认 1000，必须不小于默认页大小。

大工作区下路径排序为一次 `O(n log n)` 操作；读取和解析由有界 worker 池控制内存和 CPU。查询索引使用不可变 map 与预排序符号切片，查询延迟不随更新阻塞。

## 落盘格式与崩溃恢复

存储目录包含：

- `manifest.json`：原子发布的当前版本清单；
- `index-<version>.json`：完整快照数据；
- `.manifest.json.tmp-*`、`.index-<version>.json.tmp-*`：尚未发布的临时文件；
- `.index.lock`：Unix 下的 `flock` 咨询锁。

保存顺序：

1. 获取进程内互斥和进程间排他咨询锁；
2. 将完整 JSON 写入临时文件；
3. `fsync` 临时文件并 rename 为版本数据文件；
4. 同步目录；
5. 将新清单写入临时文件，`fsync` 后 rename 覆盖 `manifest.json`；
6. 同步目录，再最佳努力清理旧版本和临时文件。

rename 在同一目录内是原子的。因此：

- 写数据文件时崩溃：旧清单和旧数据仍可用；
- 写清单时崩溃：系统只能看到旧清单或新清单，不会看到半截清单；
- 加载到的任一版本都包含完整文件记录和符号；
- 已由新清单完整发布但损坏的数据文件会返回明确错误，而不是返回部分索引。

Windows 构建仍通过同目录原子 rename 保证发布原子性，但不提供 `flock`；跨进程同时写入时依赖文件系统原子发布，不依赖多写者合并。

## API 语义摘要

- `NewEngine`：从空版本开始；
- `NewEngineWithStore`：启动时尝试加载磁盘版本，无索引则从空版本开始；
- `Update`：增量构建并原子提交；
- `UpdateAndSave`：先提交内存版本，再持久化；
- `Save`：保存当前已提交版本；
- `Snapshot`：取得当前不可变快照；
- `Search`：按名称、前缀、种类和文件查询；
- `Outline`：获取单文件大纲；
- `Close`：释放资源。

当前版本不会自动 watch 文件系统；调用方在增删改后调用 `Update()` 即可。
