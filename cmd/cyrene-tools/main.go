package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"cyrene-go-plugins/internal/approval"
	"cyrene-go-plugins/internal/evidence"
	"cyrene-go-plugins/internal/mcpserver"
	"cyrene-go-plugins/internal/upstream"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println("cyrene-tools [mcp | explain-approval | draft-eval-issues | review-eval-fix]\nMCP uses stdio. Single-call commands read one JSON object from stdin and return one JSON object on stdout.")
		return nil
	}
	if len(args) > 1 {
		return errors.New("see cyrene-tools --help")
	}
	mode := "mcp"
	if len(args) == 1 {
		mode = args[0]
	}
	if mode != "mcp" && mode != "explain-approval" && mode != "draft-eval-issues" && mode != "review-eval-fix" {
		return errors.New("unknown command; see cyrene-tools --help")
	}
	tools, err := config(mode != "explain-approval")
	if err != nil {
		return err
	}
	if mode == "mcp" {
		return mcpserver.New(tools).Run(ctx, &mcp.StdioTransport{})
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, (96<<10)+1))
	if err != nil {
		return err
	}
	var out any
	switch mode {
	case "explain-approval":
		input, err := mcpserver.Decode[approval.Input](raw)
		if err != nil {
			return err
		}
		out, err = approval.Explain(ctx, input, tools.Narrator)
		if err != nil {
			return err
		}
	case "draft-eval-issues":
		input, err := mcpserver.Decode[evidence.Input](raw)
		if err != nil {
			return err
		}
		out, err = tools.Triage.Draft(ctx, input)
		if err != nil {
			return err
		}
	case "review-eval-fix":
		input, err := mcpserver.Decode[evidence.ReviewInput](raw)
		if err != nil {
			return err
		}
		out, err = tools.Triage.Review(ctx, input)
		if err != nil {
			return err
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(out)
}

func env(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

func config(enableTriage bool) (mcpserver.Tools, error) {
	var tools mcpserver.Tools
	if url, modelID, token := env("CYRENE_MODEL_BASE_URL"), env("CYRENE_MODEL_ID"), env("CYRENE_MODEL_TOKEN"); url != "" || modelID != "" || token != "" {
		if url == "" || modelID == "" {
			return tools, errors.New("set both CYRENE_MODEL_BASE_URL and CYRENE_MODEL_ID")
		}
		client, err := upstream.New(url, token)
		if err != nil {
			return tools, fmt.Errorf("model configuration: %w", err)
		}
		tools.Narrator = upstream.Model{Client: client, ID: modelID}
		tools.Triage.Model = tools.Narrator
	}
	if !enableTriage {
		return tools, nil
	}
	switch env("CYRENE_ECHO_API_MODE") {
	case "", "legacy":
	case "workspace":
		tools.Triage.Echo.WorkspaceAPI = true
	default:
		return tools, errors.New("CYRENE_ECHO_API_MODE must be legacy or workspace")
	}
	if url, token := env("CYRENE_ECHO_URL"), env("CYRENE_ECHO_TOKEN"); url != "" || token != "" {
		if url == "" || token == "" {
			return tools, errors.New("set CYRENE_ECHO_URL and a dedicated CYRENE_ECHO_TOKEN together")
		}
		client, err := upstream.New(url, token)
		if err != nil {
			return tools, fmt.Errorf("Echo configuration: %w", err)
		}
		tools.Triage.Echo.Client = client
	}
	if repo, token := env("CYRENE_GITHUB_REPOSITORY"), env("CYRENE_GITHUB_TOKEN"); repo != "" || token != "" {
		if !evidence.ValidRepository(repo) {
			return tools, errors.New("CYRENE_GITHUB_REPOSITORY must have owner/repo format")
		}
		client, err := upstream.New("https://api.github.com", token)
		if err != nil {
			return tools, fmt.Errorf("GitHub configuration: %w", err)
		}
		tools.Triage.GitHub = evidence.GitHub{Client: client, Repository: repo}
	}
	return tools, nil
}
