// Package colstore 提供本地列式表存储：列数据按列分文件存放，
// 表结构等元数据存放在 SQLite 中，支持结构版本与回滚。
package colstore

import "errors"

// ColumnType 列类型：整数、浮点、字符串。
type ColumnType int

const (
	TypeInt ColumnType = iota + 1
	TypeFloat
	TypeString
)

func (t ColumnType) valid() bool {
	return t == TypeInt || t == TypeFloat || t == TypeString
}

// Column 描述当前结构中的一列。
type Column struct {
	Name string
	Type ColumnType
}

// Value 一个单元格的值。Null 为 true 表示空值；
// 字符串的空串 "" 与空值是不同的。
type Value struct {
	typ   ColumnType
	Null  bool
	Int   int64
	Float float64
	Str   string
}

// IntValue 构造整数值。
func IntValue(v int64) Value { return Value{typ: TypeInt, Int: v} }

// FloatValue 构造浮点值。
func FloatValue(v float64) Value { return Value{typ: TypeFloat, Float: v} }

// StringValue 构造字符串值（空串也是非空值）。
func StringValue(v string) Value { return Value{typ: TypeString, Str: v} }

// NullValue 构造空值。
func NullValue() Value { return Value{Null: true} }

// Row 一行数据：列名 -> 值。
type Row map[string]Value

// MaxColumns 一张表最多 500 列。
const MaxColumns = 500

// KeepVersions 每张表保留的最近结构版本数（含当前版本）。
const KeepVersions = 20

var (
	ErrTableExists     = errors.New("colstore: table already exists")
	ErrTableNotFound   = errors.New("colstore: table not found")
	ErrTooManyColumns  = errors.New("colstore: too many columns")
	ErrTooFewColumns   = errors.New("colstore: table needs at least one column")
	ErrDuplicateColumn = errors.New("colstore: duplicate column name")
	ErrColumnNotFound  = errors.New("colstore: column not found")
	ErrUnknownColumn   = errors.New("colstore: unknown column in row")
	ErrTypeMismatch    = errors.New("colstore: value type does not match column type")
	ErrVersionNotFound = errors.New("colstore: version not found or not retained")
	ErrInvalidName     = errors.New("colstore: invalid table or column name")
)
