package httpapi

type financialBasisProjection struct {
	kind, id, no, namespace string
	version                 int
}

// targets 按SQL追加列的固定顺序提供来源扫描地址。
// 参数：无。
// 返回值：依据类型、本地ID/编号/版本和客户命名空间的地址。
func (b *financialBasisProjection) targets() []any {
	return []any{&b.kind, &b.id, &b.no, &b.version, &b.namespace}
}

// decorate 为原财务响应补充显式来源元数据，保持合同编号和金额语义。
// 参数：item 为已有通过原授权范围读取的业务响应。
// 返回值：保留原字段并追加依据类型、本地单据及客户命名空间的响应。
func (b financialBasisProjection) decorate(item map[string]any) map[string]any {
	item["basis_type"] = b.kind
	item["local_document_id"] = b.id
	item["local_document_no"] = b.no
	item["local_document_version"] = b.version
	item["customer_namespace"] = b.namespace
	return item
}
