# Cyrene Approval & Eval Issue Tools

面向 Cyrene 工作台的 Go 工具集，支持 stdio MCP 和单次 JSON 命令行调用。

- **审批变更说明** · 比较前后参数和修订号，将脱敏差异附到 Navigator 审批卡片。
- **评测工单草稿** · 汇总 Echo 失败样本，搜索已有 GitHub Issue，生成 Markdown 草稿。
- **修复复核** · 比较修复前后的 Echo 评测，列出恢复、仍失败和新增失败的样本。

## 快速体验

需要 Go 1.25 或更新版本。在仓库根目录运行以下命令查看示例审批差异。

```sh
go build -buildvcs=false -o bin/cyrene-tools ./cmd/cyrene-tools
./bin/cyrene-tools explain-approval < examples/approval.json
```

[详细文档](docs/README.md) · 配置、MCP 接入和工作流示例
