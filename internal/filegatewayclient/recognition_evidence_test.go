package filegatewayclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRecognitionEvidenceUsesDedicatedConfidentialPurpose 验证真实HTTP上传包含独立财务用途及机密级别。
// 参数：t为本地HTTP适配器契约测试。
// 返回值：无，上传用途、应用或摘要请求身份丢失时失败。
func TestRecognitionEvidenceUsesDedicatedConfidentialPurpose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e := r.ParseMultipartForm(1 << 20); e != nil {
			t.Fatal(e)
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("purpose") != "settlement.recognition.evidence" || r.FormValue("classification") != "CONFIDENTIAL" || r.FormValue("application_id") != "app" {
			t.Errorf("financial upload purpose drift")
		}
		if r.Header.Get("Authorization") != "Bearer isolated-token" {
			t.Error("dedicated gateway credential not sent")
		}
		_, _ = io.WriteString(w, `{"data":{"file_id":"real-reference"}}`)
	}))
	defer server.Close()
	client, e := New(server.URL, server.Client(), func(context.Context) (string, error) { return "isolated-token", nil })
	if e != nil {
		t.Fatal(e)
	}
	file, e := client.Upload(context.Background(), "recognition-upload", "app", "RECOGNITION_EVIDENCE", "proof.pdf", "application/pdf", strings.NewReader("%PDF-1.4\nisolated proof"))
	if e != nil || file != "real-reference" {
		t.Fatalf("upload=%s err=%v", file, e)
	}
}
