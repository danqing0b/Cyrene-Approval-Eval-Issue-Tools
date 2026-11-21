package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"cyrene-go-plugins/internal/approval"
	"cyrene-go-plugins/internal/evidence"
	"cyrene-go-plugins/internal/upstream"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	ApprovalTool = "cyrene_explain_approval"
	IssueTool    = "cyrene_draft_eval_issues"
	ReviewTool   = "cyrene_review_eval_fix"
)

type Tools struct {
	Narrator upstream.Narrator
	Triage   evidence.Service
}

func New(tools Tools) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "cyrene-go-tools", Version: "0.2.0"}, nil)
	slots := make(chan struct{}, 4)
	register[approval.Input](server, ApprovalTool,
		"Compute the diff between two host-projected approval snapshots. Return a summary and details compatible with Navigator work_request_approval.",
		slots, func(ctx context.Context, input approval.Input) (any, error) {
			return approval.Explain(ctx, input, tools.Narrator)
		})
	register[evidence.Input](server, IssueTool,
		"Read failed samples from a completed Echo evaluation, find related GitHub issues and return evidence-backed draft issues. Does not post anything.",
		slots, func(ctx context.Context, input evidence.Input) (any, error) {
			return tools.Triage.Draft(ctx, input)
		})
	register[evidence.ReviewInput](server, ReviewTool,
		"Compare two completed Echo evaluations. Verify sample comparability, summarize recovered and failing samples, and prepare a follow-up draft for an optional GitHub issue.",
		slots, func(ctx context.Context, input evidence.ReviewInput) (any, error) {
			return tools.Triage.Review(ctx, input)
		})
	return server
}

var inputs = map[string]string{
	ApprovalTool: `{"type":"object","additionalProperties":false,"required":["kind","workspaceId","target","expectedRevision","before","after"],"$defs":{"snapshot":{"type":"object","additionalProperties":false,"required":["revision","params"],"properties":{"revision":{"type":"integer","minimum":0},"params":{"type":"object","description":"Host-projected non-secret parameter values."}}}},"properties":{"kind":{"type":"string","description":"Operation kind from the host preview, e.g. pipelines.patch."},"workspaceId":{"type":"string"},"pipelineId":{"type":"string"},"target":{"type":"object","additionalProperties":false,"required":["kind","id"],"properties":{"kind":{"type":"string"},"id":{"type":"string"}}},"expectedRevision":{"type":"integer","minimum":0,"description":"Revision pinned by the original operation preview."},"before":{"$ref":"#/$defs/snapshot"},"after":{"$ref":"#/$defs/snapshot"},"useAI":{"type":"boolean"}}}`,
	IssueTool:    `{"type":"object","additionalProperties":false,"required":["runId"],"properties":{"runId":{"type":"string","format":"uuid"},"maxSamples":{"type":"integer","minimum":1,"maximum":300,"default":120},"maxGroups":{"type":"integer","minimum":1,"maximum":8,"default":6},"useAI":{"type":"boolean"}}}`,
	ReviewTool:   `{"type":"object","additionalProperties":false,"required":["baselineRunId","candidateRunId"],"properties":{"baselineRunId":{"type":"string","format":"uuid","description":"Echo run cited in the original issue."},"candidateRunId":{"type":"string","format":"uuid","description":"Completed Echo run for the fix."},"issueNumber":{"type":"integer","minimum":1,"maximum":2147483647,"description":"Issue number in the host-configured repository."},"groupSignature":{"type":"string","pattern":"^cyrene-[a-f0-9]{20}$","description":"Scope the comparison to one failure group from the original issue draft."},"candidateModelRef":{"type":"string","maxLength":200,"description":"Require this exact model reference on candidate samples."},"maxSamples":{"type":"integer","minimum":1,"maximum":300,"default":300,"description":"Sample read limit per run."},"useAI":{"type":"boolean"}}}`,
}

func register[T any](server *mcp.Server, name, description string, slots chan struct{}, run func(context.Context, T) (any, error)) {
	server.AddTool(&mcp.Tool{
		Name: name, Description: description, InputSchema: json.RawMessage(inputs[name]),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		input, err := Decode[T](req.Params.Arguments)
		if err != nil {
			return toolError(err), nil
		}
		result, err := run(ctx, input)
		if err != nil {
			return toolError(err), nil
		}
		body, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{
			StructuredContent: result,
			Content:           []mcp.Content{&mcp.TextContent{Text: string(body)}},
		}, nil
	})
}

func Decode[T any](data []byte) (T, error) {
	var value T
	if len(data) == 0 || len(data) > 96<<10 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return value, errors.New("expected a JSON argument object within the size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("invalid arguments: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return value, errors.New("expected exactly one JSON argument object")
	}
	return value, nil
}

func toolError(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}
