# colstore

本地列式表存储库，供上层数仓服务直接以库的方式调用（无接口层）。

- 列数据按列分文件存放在本地目录，读指定列时不打开其他列的文件。
- 表结构等元数据存放在 SQLite（`meta.db`）。
- 结构变更（加列 / 删列 / 回滚）只动元数据，不重写已有数据，耗时与行数无关。
- 保留最近 20 个结构版本，可回滚到其中任意非当前版本。

## 用法

```go
s, _ := colstore.Open("/data/warehouse")
defer s.Close()

// 建表（产生版本 1）
s.CreateTable("orders", []colstore.Column{
    {Name: "id", Type: colstore.TypeInt},
    {Name: "amount", Type: colstore.TypeFloat},
    {Name: "note", Type: colstore.TypeString},
})

t, _ := s.Table("orders")
defer t.Close()

// 追加（整批校验，任一行非法则整批拒绝；缺省的列记为空）
t.Append([]colstore.Row{
    {"id": colstore.IntValue(1), "amount": colstore.FloatValue(9.9)},
    {"id": colstore.IntValue(2)}, // amount、note 为空值
})

t.AddColumn(colstore.Column{Name: "tag", Type: colstore.TypeString}) // 版本 2
t.DropColumn("tag")                                                 // 版本 3
t.Rollback(2)                                                       // 版本 4 = 版本 2 的结构

// 按当前结构读出指定的列，行按写入顺序排列
rows, _ := t.Read([]string{"id", "note"})

// 批量扫描用列式接口，返回与请求列一一对应的列数据
cols, _ := t.ReadColumns([]string{"id"})
```

值用 `colstore.IntValue / FloatValue / StringValue / NullValue` 构造；
空值可以写给任何类型的列；字符串空串 `""` 与空值是不同的。

## 存储布局

```
<dir>/<table>/
  meta.db        SQLite：版本快照、行数、列 ID 计数器、行区间映射
  c<id>.dat      一个列实例的数据文件
```

- 每个列实例有表内唯一、单调递增的列 ID；同名再加列是全新实例，读不到旧数据。
- 列文件格式：6 字节头（magic、格式版本、列类型）+ 逐行记录。
  整数 / 浮点定长 9 字节（1 字节空值标记 + 8 字节值）；字符串变长
  （1 字节空值标记，非空时跟 4 字节长度 + 内容）。
- 列文件只追加。某列不在结构里时写入的行，其文件里没有对应记录；
  `segments` 元数据表记录每个列文件的记录段对应的全表行区间，
  区间外的行读出为空。因此加列、删列、回滚都不需要触碰已有数据。
- 每次结构变更产生新版本（版本号从 1 递增），只保留最近 20 个版本；
  不再被任何保留版本引用的列，其数据文件在该次操作结束时删除。

## 测试

```sh
go test .          # 全部规则测试 + 随机模型测试 + 200 万行性能测试
go test -race .    # 竞态检测
```

- `rules_test.go`：建表 / 追加 / 版本号 / 20 版本窗口 / 删列规则 / 空串与空值 /
  只打开被请求的列文件。
- `rollback_gc_test.go`：回滚的结构与数据语义、同名再加列、失引用列文件的磁盘回收、
  关闭重开一致性。
- `random_test.go`：独立模型逐步对比，3000 次随机混合操作（追加、非法批次、加列、
  删列、同名再加、回滚、关闭重开），每步全量读出比对；结束时校验磁盘上的列文件
  集合与存活列一致。
- `perf_test.go`：50 列 × 200 万行，加列 / 删列 / 回滚各 < 100ms，
  单列全量读 < 1s。
