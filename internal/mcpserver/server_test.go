package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cyrene-go-plugins/internal/evidence"
	"cyrene-go-plugins/internal/upstream"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolBoundaries(t *testing.T) {
	const runID = "11111111-1111-4111-8111-111111111111"
	const suiteID = "22222222-2222-4222-8222-222222222222"
	const sampleID = "33333333-3333-4333-8333-333333333333"
	// Serve versioned Echo, Exchange and GitHub fixtures over the same local listener.
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/chat/completions":
			if r.Method != http.MethodPost {
				t.Errorf("model method: %s", r.Method)
			}
			body, _ := io.ReadAll(r.Body)
			for _, text := range []string{"raw-evaluation-value", "private-api-secret", runID, sampleID} {
				if strings.Contains(string(body), text) {
					t.Errorf("model received private data: %s", text)
				}
			}
			fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"请核对这些变化是否和预览一致。"}}]}`)
		case "/api/v1/evaluation-runs/" + runID:
			if r.Method != http.MethodGet {
				t.Errorf("Echo method: %s", r.Method)
			}
			fmt.Fprintf(w, `{"id":%q,"suiteId":%q,"state":"SUCCEEDED","resourceVersion":2}`, runID, suiteID)
		case "/api/v1/evaluation-runs/" + runID + "/samples":
			if r.Method != http.MethodGet || r.URL.Query().Get("only_passed") != "false" || r.Header.Get("Authorization") != "Bearer echo-read" {
				t.Errorf("expected scoped Echo read, got %s %s", r.Method, r.URL.Path)
			}
			fmt.Fprintf(w, `[{"id":%q,"runId":%q,"sampleIndex":1,"evaluator":"exact_match.v1","inputDigest":"sha256:%s","passed":false,"score":0,"expected":"expected","actual":"raw-evaluation-value","modelRef":"model-version://sha256/example"}]`, sampleID, runID, strings.Repeat("a", 64))
		case "/search/issues":
			query := r.URL.Query().Get("q")
			if r.Method != http.MethodGet || !strings.HasPrefix(query, "repo:do/example is:issue in:body ") || strings.Contains(query, runID) {
				t.Errorf("unexpected GitHub lookup %q", query)
			}
			signature := strings.Trim(strings.TrimPrefix(query, "repo:do/example is:issue in:body "), "\"")
			fmt.Fprintf(w, `{"total_count":1,"items":[{"number":3,"title":"Known failure","state":"open","html_url":"https://github.com/do/example/issues/3","body":%q}]}`, "Cyrene signature: "+signature)
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer host.Close()
	newClient := func(path, token string) *upstream.Client {
		t.Helper()
		c, err := upstream.New(host.URL+path, token)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	model := upstream.Model{Client: newClient("/v1", ""), ID: "fixture-model"}
	service := evidence.Service{
		Echo:   evidence.Echo{Client: newClient("", "echo-read")},
		GitHub: evidence.GitHub{Client: newClient("", ""), Repository: "do/example"},
		Model:  model,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(Tools{Narrator: model, Triage: service}).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "cyrene-smoke", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	list, err := session.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 3 {
		t.Fatalf("tools/list: %v %v", list, err)
	}

	approval := `{"kind":"pipelines.patch","workspaceId":"local","pipelineId":"demo","target":{"kind":"pipeline","id":"demo"},"expectedRevision":7,"before":{"revision":7,"params":{"actualCount":9007199254740993,"apiKey":"private-api-secret","equal":1}},"after":{"revision":8,"params":{"actualCount":9007199254740994,"apiKey":"new-private-api-secret","equal":1.0}},"useAI":true}`
	approvedInput, err := session.CallTool(ctx, &mcp.CallToolParams{Name: ApprovalTool, Arguments: json.RawMessage(approval)})
	if err != nil || approvedInput.IsError {
		t.Fatalf("approval call: %v %v", approvedInput, err)
	}
	text := approvedInput.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{"9007199254740993", "9007199254740994", "[redacted]", "generated"} {
		if !strings.Contains(text, want) {
			t.Errorf("approval omitted %q: %s", want, text)
		}
	}
	for _, forbidden := range []string{"private-api-secret", "/equal"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("approval exposed %q: %s", forbidden, text)
		}
	}
	stale := strings.Replace(approval, `"expectedRevision":7`, `"expectedRevision":6`, 1)
	rejected, err := session.CallTool(ctx, &mcp.CallToolParams{Name: ApprovalTool, Arguments: json.RawMessage(stale)})
	if err != nil || !rejected.IsError || !strings.Contains(rejected.Content[0].(*mcp.TextContent).Text, "revision mismatch") {
		t.Fatalf("stale revision: %v %v", rejected, err)
	}
	drafts, err := session.CallTool(ctx, &mcp.CallToolParams{Name: IssueTool, Arguments: map[string]any{"runId": runID, "useAI": true}})
	if err != nil || drafts.IsError {
		t.Fatalf("issue draft call: %v %v", drafts, err)
	}
	text = drafts.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{"add_evidence_to_open_match", sampleID, "model-version://sha256/example", "generated"} {
		if !strings.Contains(text, want) {
			t.Errorf("draft omitted %q: %s", want, text)
		}
	}
	if strings.Contains(text, "raw-evaluation-value") {
		t.Errorf("draft exposed raw evaluation output")
	}
}
