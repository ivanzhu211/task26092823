package colstore

import (
	"bufio"
	"encoding/binary"
	"io"
	"math"
	"os"
)

// 列数据文件格式：记录顺序追加，每条记录为
//
//	tag(1 字节) + 负载
//
// tag: 0=NULL（无负载） 1=int64(8 字节 LE) 2=float64(8 字节 LE)
//
//	3=string(uvarint 长度 + 原始字节，空串长度为 0，与 NULL 不同)
const (
	tagNull   = 0
	tagInt    = 1
	tagFloat  = 2
	tagString = 3
)

// colWriter 向单列数据文件顺序追加记录。
type colWriter struct {
	f  *os.File
	bw *bufio.Writer
	n  int64 // 已写入的记录数（含打开前文件里已有的）
}

// openColWriter 以追加模式打开（必要时创建）列文件。
// existing 为文件中已有的记录数。
func openColWriter(path string, existing int64) (*colWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &colWriter{f: f, bw: bufio.NewWriterSize(f, 1<<20), n: existing}, nil
}

func (w *colWriter) write(v Value) error {
	var buf [9]byte
	switch v.Kind {
	case KindNull:
		if err := w.bw.WriteByte(tagNull); err != nil {
			return err
		}
	case KindInt:
		buf[0] = tagInt
		binary.LittleEndian.PutUint64(buf[1:], uint64(v.I))
		if _, err := w.bw.Write(buf[:9]); err != nil {
			return err
		}
	case KindFloat:
		buf[0] = tagFloat
		binary.LittleEndian.PutUint64(buf[1:], math.Float64bits(v.F))
		if _, err := w.bw.Write(buf[:9]); err != nil {
			return err
		}
	case KindString:
		if err := w.bw.WriteByte(tagString); err != nil {
			return err
		}
		n := binary.PutUvarint(buf[:], uint64(len(v.S)))
		if _, err := w.bw.Write(buf[:n]); err != nil {
			return err
		}
		if _, err := w.bw.WriteString(v.S); err != nil {
			return err
		}
	}
	w.n++
	return nil
}

// flush 把缓冲数据刷入文件（不 fsync，不关闭）。
func (w *colWriter) flush() error { return w.bw.Flush() }

func (w *colWriter) close() error {
	if err := w.bw.Flush(); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}

// colReader 顺序读取单列数据文件。
type colReader struct {
	f  *os.File
	br *bufio.Reader
}

func openColReader(path string) (*colReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &colReader{f: f, br: bufio.NewReaderSize(f, 1<<20)}, nil
}

// read 读取下一条记录；文件结束时返回 io.EOF。
func (r *colReader) read() (Value, error) {
	tag, err := r.br.ReadByte()
	if err != nil {
		return Null(), err
	}
	switch tag {
	case tagNull:
		return Null(), nil
	case tagInt:
		var buf [8]byte
		if _, err := io.ReadFull(r.br, buf[:]); err != nil {
			return Null(), err
		}
		return Int(int64(binary.LittleEndian.Uint64(buf[:]))), nil
	case tagFloat:
		var buf [8]byte
		if _, err := io.ReadFull(r.br, buf[:]); err != nil {
			return Null(), err
		}
		return Float(math.Float64frombits(binary.LittleEndian.Uint64(buf[:]))), nil
	case tagString:
		ln, err := binary.ReadUvarint(r.br)
		if err != nil {
			return Null(), err
		}
		buf := make([]byte, ln)
		if _, err := io.ReadFull(r.br, buf); err != nil {
			return Null(), err
		}
		return String(string(buf)), nil
	}
	return Null(), io.ErrUnexpectedEOF
}

func (r *colReader) close() error { return r.f.Close() }
