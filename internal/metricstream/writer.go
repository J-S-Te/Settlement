// Package metricstream 输出兼容原事实行的有界完整快照协议。
package metricstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
)

const MaxRow = 64 << 10
const MaxFrame = 1 << 20
const MaxRows = 256

type Writer struct {
	out                    io.Writer
	digest                 hash.Hash
	rows                   []json.RawMessage
	bytes, sequence, count int
}

// New 输出绑定可信租户与来源的开始帧。
// 参数：out 为响应流，metadata 为不含事实行的原v1快照元数据。
// 返回值：有界写入器；开始帧输出失败时返回错误。
func New(out io.Writer, metadata any) (*Writer, error) {
	w := &Writer{out: out, digest: sha256.New()}
	if err := w.frame(map[string]any{"type": "begin", "metadata": metadata}); err != nil {
		return nil, err
	}
	return w, nil
}

// frame 写入单个有界JSON帧并传播短写及网络错误。
// 参数：value 为完整帧对象。
// 返回值：编码、帧超限或输出失败时返回错误。
func (w *Writer) frame(value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > MaxFrame {
		return errors.New("metric frame exceeds limit")
	}
	raw = append(raw, '\n')
	n, err := w.out.Write(raw)
	if err == nil && n != len(raw) {
		return io.ErrShortWrite
	}
	return err
}

// Add 接收一行事实并在行数或字节边界前刷出上一块。
// 参数：row 为兼容v1的单一事实。
// 返回值：行超限或输出失败时错误；失败后调用方不得输出结束帧。
func (w *Writer) Add(row any) error {
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if len(raw) > MaxRow {
		return errors.New("metric row exceeds limit")
	}
	if len(w.rows) >= MaxRows || w.bytes+len(raw)+1 > MaxFrame-128 {
		if err := w.Flush(); err != nil {
			return err
		}
	}
	w.rows = append(w.rows, json.RawMessage(raw))
	w.bytes += len(raw) + 1
	w.count++
	_, _ = w.digest.Write(raw)
	_, _ = w.digest.Write([]byte{'\n'})
	return nil
}

// Flush 输出当前非空块，维持连续序号和有限内存。
// 参数：无。
// 返回值：输出失败时返回错误；空块不输出。
func (w *Writer) Flush() error {
	if len(w.rows) == 0 {
		return nil
	}
	w.sequence++
	if err := w.frame(map[string]any{"type": "chunk", "sequence": w.sequence, "rows": w.rows}); err != nil {
		return err
	}
	w.rows = nil
	w.bytes = 0
	return nil
}

// End 仅供源快照成功提交后输出完成计数与摘要。
// 参数：updated/started 为源事实最终更新时间和采集起点，可为空。
// 返回值：输出失败返回错误，消费者必须拒绝缺失或截断的结束帧。
func (w *Writer) End(updated, started string) error {
	if err := w.Flush(); err != nil {
		return err
	}
	return w.frame(struct {
		Type    string `json:"type"`
		Count   int    `json:"row_count"`
		SHA     string `json:"sha256"`
		Updated string `json:"source_updated_at,omitempty"`
		Started string `json:"collection_started_at,omitempty"`
	}{"end", w.count, hex.EncodeToString(w.digest.Sum(nil)), updated, started})
}
