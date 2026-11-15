package service

import "errors"

// feedbackError 仅承载代码明确生成的公开业务原因，不接收外部异常文本。
type feedbackError struct {
	kind       error
	reasonCode string
	message    string
	issues     []feedbackIssue
}

type feedbackIssue struct {
	Field   string `json:"field"`
	Row     int    `json:"row,omitempty"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

// Error 用途：返回受控业务原因，供日志和既有错误断言使用。
// 参数：无。
// 返回值：不含底层异常的业务说明。
func (e *feedbackError) Error() string { return e.message }

// Unwrap 用途：保留原有权限、输入与冲突错误分类。
// 参数：无。
// 返回值：原错误类型，支持 errors.Is。
func (e *feedbackError) Unwrap() error { return e.kind }

// 用途：构造只包含受控文案和字段坐标的业务拒绝。
// 参数：kind 为既有分类，reason 为稳定原因，message 为代码生成说明，issues 为字段问题。
// 返回值：可保留原错误匹配并被HTTP安全公开的错误。
func businessFeedback(kind error, reason, message string, issues ...feedbackIssue) error {
	return &feedbackError{kind: kind, reasonCode: reason, message: message, issues: issues}
}

// BusinessFeedback 用途：只读取显式安全载体，避免直接公开任意底层错误。
// 参数：err 为服务层错误，允许外层包装。
// 返回值：安全说明、结构化详情与是否可公开；未标记错误返回空值和 false。
func BusinessFeedback(err error) (string, map[string]any, bool) {
	var feedback *feedbackError
	if !errors.As(err, &feedback) {
		return "", nil, false
	}
	details := map[string]any{"reason_code": feedback.reasonCode}
	if len(feedback.issues) > 0 {
		details["issues"] = feedback.issues
	}
	return feedback.message, details, true
}
