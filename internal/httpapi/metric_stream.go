package httpapi

import (
	"net/http"
)

type metricResponseWriter struct {
	http.ResponseWriter
	written bool
}

// Write 记录是否已经输出协议字节，防止向中断流追加成功或错误包络。
// 参数：data 为当前协议帧。
// 返回值：实际写入字节数与底层错误。
func (w *metricResponseWriter) Write(data []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(data)
}
