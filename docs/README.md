# 使用与集成

[返回项目首页](../README.md)

一个 `cyrene-tools` 可执行文件提供三个工具。MCP 客户端使用标准输入输出连接，宿主也可以通过同一个可执行文件发起单次 JSON 调用。

| MCP 工具 | CLI 子命令 | 用途 |
| --- | --- | --- |
| `cyrene_explain_approval` | `explain-approval` | 比较资源修订和参数，输出摘要与结构化差异 |
| `cyrene_draft_eval_issues` | `draft-eval-issues` | 对失败样本分组、搜索已有 Issue 并生成草稿 |
| `cyrene_review_eval_fix` | `review-eval-fix` | 对比已完成的评测，生成样本级复核报告与后续草稿 |

## 构建与运行

需要 Go 1.25 或更新版本。以下命令在仓库根目录运行。审批示例可直接执行。工单相关示例需要先配置 Echo，并替换两个文件里的示例 run ID。`review-eval-fix` 示例里的 `issueNumber` 也可以删除。

```sh
go build -buildvcs=false -o bin/cyrene-tools ./cmd/cyrene-tools
./bin/cyrene-tools explain-approval < examples/approval.json
./bin/cyrene-tools draft-eval-issues < examples/eval-issues.json
./bin/cyrene-tools review-eval-fix < examples/review-eval-fix.json
```

每个 CLI 子命令从标准输入读取一个 JSON 对象并向标准输出写入一个 JSON 对象。诊断写入标准错误。直接运行 `cyrene-tools` 或指定 `mcp` 子命令都将启动 stdio MCP 服务。 [审批示例](../examples/approval.json) · [工单草稿示例](../examples/eval-issues.json) · [修复复核示例](../examples/review-eval-fix.json)

### MCP 客户端

支持 `mcpServers` 配置的客户端可使用以下示例。替换 `command` 为可执行文件的绝对路径，并通过客户端的环境配置向子进程注入所需变量。

```json
{
  "mcpServers": {
    "cyrene-approval-eval-issue-tools": {
      "command": "/absolute/path/to/cyrene-tools",
      "args": ["mcp"]
    }
  }
}
```

### 环境变量

| 变量 | 配置方式 |
| --- | --- |
| `CYRENE_ECHO_URL`、`CYRENE_ECHO_TOKEN` | 两项评测工具必需。设置 Echo 服务基址和可读取目标工作区数据的 Bearer 凭据。 |
| `CYRENE_ECHO_API_MODE` | 默认 `legacy`，使用 `/api/v1`。`workspace` 使用 `/internal/workspace/v1`，并要求凭据映射到目标组织与 Workspace。 |
| `CYRENE_GITHUB_REPOSITORY` | 可选。以 `owner/repo` 固定 Issue 查重和复核的目标仓库。 |
| `CYRENE_GITHUB_TOKEN` | 可选。公开仓库可匿名搜索，私有仓库使用对目标仓库有读取权限的凭据。 |
| `CYRENE_MODEL_BASE_URL`、`CYRENE_MODEL_ID`、`CYRENE_MODEL_TOKEN` | 可选。模型基址通常以 `/v1` 结尾，工具追加 `/chat/completions`。配模型时基址与模型 ID 同时提供，令牌按模型服务要求设置。 |
| `CYRENE_APPROVAL_EXPLAINER_PATH` | 由 Navigator 执行宿主读取。填写可信 `cyrene-tools` 的绝对路径，以启用审批预览。 |

Echo 和模型服务支持 HTTPS。本机验证可使用回环 HTTP。GitHub 请求固定发往 `api.github.com`。服务地址、GitHub 仓库和凭据由宿主配置。一个进程使用一套凭据，部署时按授权的 Workspace 和仓库分配进程。

## 审批变更说明

从 Product API 读取最新资源和修订，再从操作预览中取得拟议参数。两份快照使用同一资源，并投影本次操作涉及的字段。为直接调用提供 `kind`、`workspaceId`、`target`、`expectedRevision`、`before` 和 `after`。关联流水线时可附上 `pipelineId`。

`expectedRevision` 必须等于 `before.revision`。若预览尚未分配新修订号，`after.revision` 可沿用当前值。工具返回 `kind`、`summary` 和 `details`。差异使用 JSON Pointer 表示路径，嵌套对象逐字段比较，数组整体比较。常见凭据字段会被遮盖，调用方应在输入中仅保留审批所需的非敏感参数。

### 接入 Navigator 审批卡片

在已接入 Go 辅助程序的 Navigator 宿主设置 `CYRENE_APPROVAL_EXPLAINER_PATH`，再通过原有的 `work_request_approval` 传入 `preview`。下面是该工具的参数示例。工作区来自活动任务。

```json
{
  "kind": "pipelines.patch",
  "summary": "将训练轮次从 2 调整为 3",
  "preview": {
    "target": {"kind": "pipeline", "id": "instruction-tuning"},
    "pipelineId": "instruction-tuning",
    "expectedRevision": 7,
    "before": {"revision": 7, "params": {"training": {"epochs": 2}}},
    "after": {"revision": 8, "params": {"training": {"epochs": 3}}}
  }
}
```

宿主调用固定的 `explain-approval` 子命令，将结果作为调用方提出的变更预览写入审批详情。审批仍由人工决策。宿主保存确定性的差异，使相同审批请求可重试。直接通过 CLI 或 MCP 传入 `useAI: true` 时，模型只接收差异路径和操作类型。若模型不可用，仍返回计算得到的差异。

## 评测工单草稿

1. 将一个已成功完成的 Echo 评测 ID 传为 `runId`。
2. 工具按评测器和可观察到的失败类别分组，记录样本 ID、输入摘要、得分及 Echo 实际保存的模型引用。没有模型引用的样本会单独计数。
3. 配置 GitHub 仓库后，工具按 `Cyrene signature` 搜索 Issue 正文，必要时按失败类别和评测器搜索标题。找到打开的匹配 Issue 时，草稿会建议补充证据。

`maxSamples` 默认 120，范围 1–300。`maxGroups` 默认 6，范围 1–8。`moreFailures` 表示样本读取达到预算时仍有记录可读。`issueSearchStatus` 标明查重覆盖情况，其中 `partial`、`unavailable` 和 `not_configured` 分别表示结果不完整、搜索不可用和未配置。分组指纹可用于去重与回查，根因仍需结合证据复核。工具返回 Markdown 草稿，由使用者在 GitHub 上确认并提交。

## 修复复核

在 Echo 中用同一套件和测试数据评测新输出。将两个成功完成的评测 ID 分别传为 `baselineRunId` 和 `candidateRunId`。`maxSamples` 默认每个 run 读取最多 300 条，可设为 1–300。

| 可选参数 | 作用 |
| --- | --- |
| `candidateModelRef` | 要求候选样本的模型引用精确匹配。记录缺失或不一致的样本列为无法比较。 |
| `groupSignature` | 按原草稿的分组指纹，只复核该组的基线失败样本。 |
| `issueNumber` | 只读查询配置仓库中的指定 Issue。提供分组指纹时同时核对 Issue 正文。 |
| `useAI` | 请求可选的复核解读，模型只接收汇总计数和覆盖状态。 |

两个 run 须使用相同的套件、执行绑定及评测器。使用 LLM 评审时，记录的评审模型与端点标识也须一致。同一输入制品按样本序号对应。制品不同则按稳定的 `sampleId` 配对，并核对测试输入和参考答案。双方的结果报告均可验证时，还会核对 Catalyst 数据集版本与包摘要。缺少目标绑定时，比较依赖双方可复核的样本输入。

报告分别统计恢复、仍失败、新增失败、仍通过和无法比较的样本，并标记样本读取是否截断。重复 ID、参考答案变化、候选样本缺失和被跳过的结果会注明原因。草稿使用样本 ID、得分和模型引用作为证据。工单发布和评测发起由原业务流程处理。

## 部署验证

1. 先运行审批示例，确认修订 `7 → 8` 的差异包含 `/training/epochs`。
2. 启用 Navigator 接入后，从工作台提交一次带预览的测试审批，检查差异展示和人工决策后的任务状态。
3. 使用 `workspace` 模式时，核对 Echo 已部署套件、run、样本及结果报告的私有读取路由，并用目标 Workspace 凭据运行两项评测工具。验证错误凭据返回 `401`、跨 Workspace 读不到目标资源，以及 Echo 未配置私有凭据映射时返回 `503`。
4. 检查工单查重状态、样本覆盖范围和可选 AI 的 `aiStatus`。评测工具的 AI 解读使用分组元数据或汇总计数。

运行仓库测试可验证 MCP 工具注册、审批差异、Issue 查重和评测复核逻辑，包括样本重排、参考答案变化和读取截断。

```sh
go test -buildvcs=false ./...
```

[Echo 公共 API](https://github.com/DoHorizon-AI/Cyrene-Echo/blob/5db51ca/contracts/product/v1/openapi.yaml) · [Echo Workspace 凭据部署](https://github.com/DoHorizon-AI/Cyrene-Echo/blob/5db51ca/docs/operations/ACA_WORKSPACE_SERVICE_AUTH.md)
