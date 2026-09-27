// Package colstore 提供本地列式表存储：列数据按列分文件存放，
// 表结构与版本元数据存放在 SQLite 中，支持结构版本回滚。
package colstore

import "errors"

// Kind 表示列/值的类型。
type Kind int

const (
	KindNull   Kind = iota // 空值（仅用于 Value，不作为列类型）
	KindInt                // 整数
	KindFloat              // 浮点
	KindString             // 字符串
)

// Value 表示一个单元格的值。Kind 为 KindNull 时表示空值（NULL），
// 与空字符串 "" 是不同的值。
type Value struct {
	Kind Kind
	I    int64
	F    float64
	S    string
}

// Null 返回空值。
func Null() Value { return Value{Kind: KindNull} }

// Int 返回整数值。
func Int(v int64) Value { return Value{Kind: KindInt, I: v} }

// Float 返回浮点值。
func Float(v float64) Value { return Value{Kind: KindFloat, F: v} }

// String 返回字符串值（空串也是非空值）。
func String(v string) Value { return Value{Kind: KindString, S: v} }

// Column 描述一列的名字与类型。
type Column struct {
	Name string
	Type Kind // KindInt / KindFloat / KindString
}

const (
	// MaxColumns 一张表允许的最大列数。
	MaxColumns = 500
	// KeepVersions 每张表保留的最近结构版本数（含当前版本）。
	KeepVersions = 20
)

var (
	ErrTableExists    = errors.New("colstore: table already exists")
	ErrTableNotFound  = errors.New("colstore: table not found")
	ErrTooManyColumns = errors.New("colstore: too many columns")
	ErrTooFewColumns  = errors.New("colstore: table must have at least one column")
	ErrDupColumn      = errors.New("colstore: duplicate column name")
	ErrNoSuchColumn   = errors.New("colstore: no such column")
	ErrUnknownColumn  = errors.New("colstore: row contains unknown column")
	ErrTypeMismatch   = errors.New("colstore: value type does not match column type")
	ErrVersionInvalid = errors.New("colstore: version is not a retained non-current version")
	ErrClosed         = errors.New("colstore: table is closed")
)
