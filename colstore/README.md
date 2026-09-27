# colstore

本地列式表存储库（Go），供上层数仓服务直接以库的形式调用，无接口层。

- 列数据按列分文件存放在本地目录，读指定列时不打开其他列的文件。
- 表结构、版本、行段映射等元数据存放在 SQLite（`modernc.org/sqlite`，纯 Go 无 cgo）。
- 改结构（加列/删列/回滚）不触碰已有数据，耗时与行数无关；最近 20 个结构版本可回滚。

## 用法

```go
s, _ := colstore.OpenStore("/data/warehouse")

// 建表（产生版本 1）
t, _ := s.CreateTable("orders", []colstore.Column{
    {Name: "id", Type: colstore.KindInt},
    {Name: "price", Type: colstore.KindFloat},
    {Name: "note", Type: colstore.KindString},
})

// 追加一批行（整批校验，任一行非法则整批拒绝）
t.Append([]map[string]colstore.Value{
    {"id": colstore.Int(1), "price": colstore.Float(9.9), "note": colstore.String("")},
    {"id": colstore.Int(2)}, // 未给的列记为空值 NULL（与空串 "" 不同）
})

// 改结构：各产生一个新版本
t.AddColumn(colstore.Column{Name: "tag", Type: colstore.KindString}) // v2
t.DropColumn("tag")                                                  // v3
t.Rollback(2)                                                        // v4，结构回到 v2

// 按当前结构读出指定列，行按写入顺序排列
rows, _ := t.Read([]string{"id", "note"})
t.Close()
```

## 规则要点

- 列类型：整数 / 浮点 / 字符串；任何值可为空；空串 `""` 与空值不同。
- 一表 1~500 列，当前结构内列名唯一。
- 建表、加列、删列、回滚各产生一个版本，版本号从 1 递增；只保留最近 20 个
  版本（含当前），只能回滚到其中非当前的版本。
- 新加的列对历史行读出为空；删掉的列从当前结构消失；同名再加是全新的列，
  读不到旧列数据。
- 回滚到版本 v 后结构与 v 完全相同（同样的列身份），v 中每列的历史数据
  原样可见；v 之后才加的列消失，但回滚到包含它的版本时数据原样回来。
- 一列不再出现在任何保留版本里时，其数据文件在该次操作结束时从磁盘删除。

## 存储布局

```
<root>/<table>/meta.db        SQLite：版本、列身份、行段映射、行数
<root>/<table>/cols/<id>.col  每列一个数据文件，按列身份 ID 命名
```

关键设计：

- **列身份（col_id）**：每列创建时分配全局递增 ID，同名再加列分配新 ID，
  因此旧列数据天然不可达，回滚又能精确找回原列。
- **行段（segments）**：每列的数据文件是只追加的记录流；元数据记录该列
  文件中每段连续记录对应的全局行号区间。追加只写文件尾 + 插一条段记录，
  改结构只动元数据，因此加列/删列/回滚都是 O(1)（与行数无关）。
- **版本即快照**：`version_cols` 记录每个版本引用的列身份与顺序；回滚 =
  用目标版本的列清单产生新版本。每次产生新版本时裁剪到最近 20 个版本，
  并删除不再被任何保留版本引用的列数据文件。
- **记录编码**：tag 字节（NULL/int64/float64/string）+ 负载；字符串带
  uvarint 长度，空串与 NULL 编码不同。

## 测试

```sh
go test ./...
```

- `colstore_test.go`：逐条覆盖业务规则（校验、整批拒绝、空串/空值、
  加列回填空、同名再加、回滚语义、版本保留与磁盘回收、关闭重开持久化）。
- `random_test.go`：3000 步随机混合追加/加列/删列/同名再加/回滚/关闭重开，
  每步读出全部结果与独立推算的模型逐格比对；结束检查磁盘无垃圾列文件。
- `perf_test.go`：50 列 × 200 万行。本机实测：加列/删列/回滚均 < 1ms
  （要求 < 100ms），读单列全部行约 70~95ms（要求 < 1s）。
