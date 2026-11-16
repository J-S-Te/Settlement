package metricstream

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

// TestCompleteStreamExceedsLegacyLimit 验证超过万行仍逐块有界且摘要精确。
// 参数：t 为隔离测试上下文。
// 返回值：无；帧、序号、计数或摘要不一致时失败。
func TestCompleteStreamExceedsLegacyLimit(t *testing.T) {
	var out bytes.Buffer
	w, err := New(&out, map[string]any{"rows": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	for i := 0; i < 10001; i++ {
		row := map[string]any{"record_id": fmt.Sprint(i), "text": strings.Repeat("a", 5000)}
		raw, _ := json.Marshal(row)
		digest.Write(raw)
		digest.Write([]byte{'\n'})
		if err := w.Add(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), []byte(`"type":"end"`)) {
		t.Fatal("premature end")
	}
	if err := w.End("", ""); err != nil {
		t.Fatal(err)
	}
	sequence, count := 0, 0
	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte{'\n'}) {
		if len(line) > MaxFrame {
			t.Fatal("oversized frame")
		}
		var frame struct {
			Type     string
			Sequence int
			Rows     []json.RawMessage
			Count    int    `json:"row_count"`
			SHA      string `json:"sha256"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			t.Fatal(err)
		}
		switch frame.Type {
		case "chunk":
			sequence++
			if frame.Sequence != sequence || len(frame.Rows) < 1 || len(frame.Rows) > MaxRows {
				t.Fatal("invalid chunk")
			}
			count += len(frame.Rows)
		case "end":
			if frame.Count != 10001 || frame.SHA != hex.EncodeToString(digest.Sum(nil)) {
				t.Fatal("invalid end")
			}
		}
	}
	if count != 10001 {
		t.Fatalf("count %d", count)
	}
}

// TestStreamRejectsOversizedRowAndShortWrite 验证单行与输出失败关闭。
// 参数：t 为隔离测试上下文。
// 返回值：无；超限或短写被吞掉时失败。
func TestStreamRejectsOversizedRowAndShortWrite(t *testing.T) {
	var out bytes.Buffer
	w, err := New(&out, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Add(strings.Repeat("a", MaxRow)); err == nil {
		t.Fatal("oversized row accepted")
	}
	if _, err := New(shortWriter{}, struct{}{}); err != io.ErrShortWrite {
		t.Fatalf("short write %v", err)
	}
}

type shortWriter struct{}

// Write 模拟无错误但未完整写入底层输出。
// 参数：data 为待写帧。
// 返回值：少写一个字节并返回nil，用于验证短写检测。
func (shortWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }
