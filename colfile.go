package colstore

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
)

// 列数据文件格式：
//   头部：magic(4) "CLSF" | formatVer(1)=1 | colType(1)
//   之后每行一条记录：
//     整数/浮点：flag(1)（0=空，1=有值）| 8 字节小端值（空值时也占 8 字节，内容为零）
//     字符串：  flag(1)（0=空，1=有值）；有值时跟 len(4) 小端 + 字节内容
// 整数/浮点定长 9 字节一行，便于顺序解码；字符串变长顺序解码。

var colFileMagic = []byte{'C', 'L', 'S', 'F'}

const colFileFormatVer = 1

const colFileHeaderSize = 6

// colWriter 以追加方式写一个列文件。
type colWriter struct {
	f   *os.File
	w   *bufio.Writer
	typ ColumnType
}

// createColFile 创建一个新的空列文件并返回追加写句柄。
func createColFile(path string, typ ColumnType) (*colWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	cw := &colWriter{f: f, w: bufio.NewWriterSize(f, 1<<20), typ: typ}
	hdr := []byte{colFileMagic[0], colFileMagic[1], colFileMagic[2], colFileMagic[3], colFileFormatVer, byte(typ)}
	if _, err := cw.w.Write(hdr); err != nil {
		f.Close()
		return nil, err
	}
	return cw, nil
}

// openColWriter 以追加模式打开已有列文件。
func openColWriter(path string, typ ColumnType) (*colWriter, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &colWriter{f: f, w: bufio.NewWriterSize(f, 1<<20), typ: typ}, nil
}

// append 写入一行记录。v.Null 表示空值；否则按列类型取对应字段。
func (cw *colWriter) append(v Value) error {
	var buf [9]byte
	switch cw.typ {
	case TypeInt, TypeFloat:
		if v.Null {
			buf[0] = 0
		} else {
			buf[0] = 1
			if cw.typ == TypeInt {
				binary.LittleEndian.PutUint64(buf[1:], uint64(v.Int))
			} else {
				binary.LittleEndian.PutUint64(buf[1:], math.Float64bits(v.Float))
			}
		}
		if _, err := cw.w.Write(buf[:]); err != nil {
			return err
		}
	case TypeString:
		if v.Null {
			if _, err := cw.w.Write([]byte{0}); err != nil {
				return err
			}
		} else {
			binary.LittleEndian.PutUint32(buf[1:5], uint32(len(v.Str)))
			buf[0] = 1
			if _, err := cw.w.Write(buf[:5]); err != nil {
				return err
			}
			if _, err := cw.w.WriteString(v.Str); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("colstore: unknown column type %d", cw.typ)
	}
	return nil
}

// flush 把缓冲数据落盘（不 fsync，调用方不要求抗中途被杀）。
func (cw *colWriter) flush() error {
	return cw.w.Flush()
}

func (cw *colWriter) close() error {
	if err := cw.w.Flush(); err != nil {
		cw.f.Close()
		return err
	}
	return cw.f.Close()
}

// readColFile 读出列文件里的全部记录（按写入顺序）。
// 调用方负责用行区间映射把记录对齐到全表行号。
func readColFile(path string, typ ColumnType) ([]Value, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return nil, err
	}
	if len(data) < colFileHeaderSize ||
		data[0] != colFileMagic[0] || data[1] != colFileMagic[1] ||
		data[2] != colFileMagic[2] || data[3] != colFileMagic[3] {
		return nil, fmt.Errorf("colstore: bad column file %s", path)
	}
	if ColumnType(data[5]) != typ {
		return nil, fmt.Errorf("colstore: column file %s type mismatch", path)
	}
	body := data[colFileHeaderSize:]
	out := make([]Value, 0, len(body)/9)
	switch typ {
	case TypeInt, TypeFloat:
		if len(body)%9 != 0 {
			return nil, fmt.Errorf("colstore: column file %s size mismatch", path)
		}
		for i := 0; i < len(body)/9; i++ {
			rec := body[i*9 : i*9+9]
			if rec[0] == 0 {
				out = append(out, Value{Null: true})
			} else {
				bits := binary.LittleEndian.Uint64(rec[1:])
				if typ == TypeInt {
					out = append(out, Value{typ: TypeInt, Int: int64(bits)})
				} else {
					out = append(out, Value{typ: TypeFloat, Float: math.Float64frombits(bits)})
				}
			}
		}
	case TypeString:
		pos := 0
		for pos < len(body) {
			flag := body[pos]
			pos++
			if flag == 0 {
				out = append(out, Value{Null: true})
				continue
			}
			if pos+4 > len(body) {
				return nil, fmt.Errorf("colstore: column file %s truncated", path)
			}
			n := binary.LittleEndian.Uint32(body[pos:])
			pos += 4
			if pos+int(n) > len(body) {
				return nil, fmt.Errorf("colstore: column file %s truncated", path)
			}
			out = append(out, Value{typ: TypeString, Str: string(body[pos : pos+int(n)])})
			pos += int(n)
		}
		if pos != len(body) {
			return nil, fmt.Errorf("colstore: column file %s has trailing bytes", path)
		}
	default:
		return nil, fmt.Errorf("colstore: unknown column type %d", typ)
	}
	return out, nil
}
