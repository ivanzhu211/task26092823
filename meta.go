package colstore

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// columnRec 元数据中一列在某个版本里的记录。
// ID 是列实例的全表唯一编号，同名新列会拿到新的 ID。
type columnRec struct {
	ID   int64
	Name string
	Type ColumnType
	Pos  int
}

// metaStore 封装一张表的 SQLite 元数据。
// 表结构：
//
//	meta(key TEXT PRIMARY KEY, value TEXT)：current_version / row_count / next_col_id
//	columns(version, col_id, name, type, pos)：每个结构版本的完整列快照
type metaStore struct {
	db *sql.DB
}

const metaSchema = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS columns (
	version INTEGER NOT NULL,
	col_id  INTEGER NOT NULL,
	name    TEXT NOT NULL,
	type    INTEGER NOT NULL,
	pos     INTEGER NOT NULL,
	PRIMARY KEY (version, col_id)
);
CREATE TABLE IF NOT EXISTS segments (
	col_id    INTEGER NOT NULL,
	seg_idx   INTEGER NOT NULL,
	start_row INTEGER NOT NULL,
	cnt       INTEGER NOT NULL,
	PRIMARY KEY (col_id, seg_idx)
);
`

// segment 描述某列文件中一段连续记录对应的全表行区间 [Start, Start+Cnt)。
// 列文件里的记录 = 该列所有 segment 按顺序拼接；不在任何 segment 里的行读作空。
type segment struct {
	Start int64
	Cnt   int64
}

func openMetaDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL;"); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// createMeta 创建元数据库并写入版本 1 的结构。
func createMeta(path string, cols []Column) (*metaStore, error) {
	db, err := openMetaDB(path)
	if err != nil {
		return nil, err
	}
	m := &metaStore{db: db}
	if _, err := db.Exec(metaSchema); err != nil {
		db.Close()
		return nil, err
	}
	tx, err := db.Begin()
	if err != nil {
		db.Close()
		return nil, err
	}
	defer tx.Rollback()
	for k, v := range map[string]int64{"current_version": 1, "row_count": 0, "next_col_id": int64(len(cols))} {
		if _, err := tx.Exec("INSERT INTO meta(key, value) VALUES(?, ?)", k, fmt.Sprint(v)); err != nil {
			db.Close()
			return nil, err
		}
	}
	for i, c := range cols {
		if _, err := tx.Exec("INSERT INTO columns(version, col_id, name, type, pos) VALUES(1, ?, ?, ?, ?)",
			int64(i), c.Name, int(c.Type), i); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		db.Close()
		return nil, err
	}
	return m, nil
}

// openMeta 打开已有元数据库。
func openMeta(path string) (*metaStore, error) {
	db, err := openMetaDB(path)
	if err != nil {
		return nil, err
	}
	return &metaStore{db: db}, nil
}

func (m *metaStore) close() error {
	return m.db.Close()
}

// loadSegments 读出所有列的行区间映射。
func (m *metaStore) loadSegments() (map[int64][]segment, error) {
	rows, err := m.db.Query("SELECT col_id, start_row, cnt FROM segments ORDER BY col_id, seg_idx")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64][]segment)
	for rows.Next() {
		var id int64
		var s segment
		if err := rows.Scan(&id, &s.Start, &s.Cnt); err != nil {
			return nil, err
		}
		out[id] = append(out[id], s)
	}
	return out, rows.Err()
}

// saveSegments 全量重写行区间映射。
func (m *metaStore) saveSegments(segs map[int64][]segment) error {
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM segments"); err != nil {
		return err
	}
	for id, list := range segs {
		for i, s := range list {
			if _, err := tx.Exec("INSERT INTO segments(col_id, seg_idx, start_row, cnt) VALUES(?, ?, ?, ?)",
				id, i, s.Start, s.Cnt); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (m *metaStore) getInt(key string) (int64, error) {
	var v int64
	err := m.db.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	return v, err
}

func (m *metaStore) setInt(key string, v int64) error {
	_, err := m.db.Exec("UPDATE meta SET value = ? WHERE key = ?", fmt.Sprint(v), key)
	return err
}

// state 读出当前版本号、行数、下一个列 ID。
func (m *metaStore) state() (curVer, rowCount, nextColID int64, err error) {
	if curVer, err = m.getInt("current_version"); err != nil {
		return
	}
	if rowCount, err = m.getInt("row_count"); err != nil {
		return
	}
	if nextColID, err = m.getInt("next_col_id"); err != nil {
		return
	}
	return
}

// schemaAt 读出某个版本的完整结构（按 pos 排序）。
func (m *metaStore) schemaAt(version int64) ([]columnRec, error) {
	rows, err := m.db.Query(
		"SELECT col_id, name, type, pos FROM columns WHERE version = ? ORDER BY pos", version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []columnRec
	for rows.Next() {
		var r columnRec
		var typ int
		if err := rows.Scan(&r.ID, &r.Name, &typ, &r.Pos); err != nil {
			return nil, err
		}
		r.Type = ColumnType(typ)
		out = append(out, r)
	}
	return out, rows.Err()
}

// versions 列出所有保留的版本号（升序）。
func (m *metaStore) versions() ([]int64, error) {
	rows, err := m.db.Query("SELECT DISTINCT version FROM columns ORDER BY version")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// liveColIDs 返回仍被保留版本引用的列 ID 集合。
func (m *metaStore) liveColIDs() (map[int64]bool, error) {
	rows, err := m.db.Query("SELECT DISTINCT col_id FROM columns")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// commitVersion 在一个事务里：写入新版本的完整结构快照、推进 current_version、
// 淘汰超出保留窗口的旧版本，并返回仍被保留版本引用的列 ID 集合。
func (m *metaStore) commitVersion(newVer int64, schema []columnRec) (live map[int64]bool, err error) {
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, r := range schema {
		if _, err := tx.Exec("INSERT INTO columns(version, col_id, name, type, pos) VALUES(?, ?, ?, ?, ?)",
			newVer, r.ID, r.Name, int(r.Type), r.Pos); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec("UPDATE meta SET value = ? WHERE key = 'current_version'", fmt.Sprint(newVer)); err != nil {
		return nil, err
	}
	// 只保留最近 KeepVersions 个版本（含当前版本）。
	if _, err := tx.Exec("DELETE FROM columns WHERE version <= ?", newVer-KeepVersions); err != nil {
		return nil, err
	}
	rows, err := tx.Query("SELECT DISTINCT col_id FROM columns")
	if err != nil {
		return nil, err
	}
	live = make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		live[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return live, nil
}
