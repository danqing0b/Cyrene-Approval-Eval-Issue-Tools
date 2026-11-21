package evidence

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"cyrene-go-plugins/internal/upstream"
)

type Input struct {
	RunID      string `json:"runId" jsonschema:"UUID of a completed Echo evaluation run."`
	MaxSamples *int   `json:"maxSamples,omitempty" jsonschema:"Maximum failing samples to read, 1 to 300. Default 120."`
	MaxGroups  *int   `json:"maxGroups,omitempty" jsonschema:"Maximum failure groups to draft, 1 to 8. Default 6."`
	UseAI      bool   `json:"useAI,omitempty" jsonschema:"Request a short AI explanation from aggregate failure evidence."`
}

type Ref struct {
	Index       int     `json:"index"`
	ID          string  `json:"id"`
	InputDigest string  `json:"inputDigest"`
	Score       float64 `json:"score"`
	ModelRef    string  `json:"modelRef,omitempty"`
}

type Group struct {
	Signature    string   `json:"signature"`
	Category     string   `json:"category"`
	Evaluator    string   `json:"evaluator"`
	FailureCount int      `json:"failureCount"`
	Models       []string `json:"modelRefs"`
	Unattributed int      `json:"unattributedSamples"`
	Samples      []Ref    `json:"sampleRefs"`
	SearchStatus string   `json:"issueSearchStatus"`
	Issues       []Issue  `json:"issueMatches"`
	Suggestion   string   `json:"suggestion"`
	Title        string   `json:"draftTitle"`
	Markdown     string   `json:"draftMarkdown"`
	AIStatus     string   `json:"aiStatus"`
	AIAnalysis   string   `json:"aiAnalysis,omitempty"`
}

type Result struct {
	RunID         string  `json:"runId"`
	SuiteID       string  `json:"suiteId"`
	RunRevision   int64   `json:"runRevision"`
	Repository    string  `json:"repository,omitempty"`
	FailuresRead  int     `json:"failuresRead"`
	MoreFailures  bool    `json:"moreFailures"`
	GroupsOmitted int     `json:"groupsOmitted"`
	Groups        []Group `json:"groups"`
}

type Service struct {
	Echo   Echo
	GitHub GitHub
	Model  upstream.Narrator
}

func (s Service) Draft(ctx context.Context, input Input) (Result, error) {
	if !uuid.MatchString(input.RunID) {
		return Result{}, errors.New("runId must be an Echo evaluation UUID")
	}
	maxSamples, maxGroups := 120, 6
	if input.MaxSamples != nil {
		maxSamples = *input.MaxSamples
	}
	if input.MaxGroups != nil {
		maxGroups = *input.MaxGroups
	}
	if maxSamples < 1 || maxSamples > 300 || maxGroups < 1 || maxGroups > 8 {
		return Result{}, errors.New("maxSamples must be 1–300 and maxGroups 1–8")
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Second)
	defer cancel()
	run, samples, more, err := s.Echo.ReadFailures(ctx, strings.ToLower(input.RunID), maxSamples)
	if err != nil {
		return Result{}, err
	}
	groups := groupFailures(run, samples)
	result := Result{RunID: run.ID, SuiteID: run.SuiteID, RunRevision: run.Revision,
		Repository: s.GitHub.Repository, FailuresRead: len(samples), MoreFailures: more, Groups: groups,
	}
	if len(groups) > maxGroups {
		result.GroupsOmitted = len(groups) - maxGroups
		result.Groups = groups[:maxGroups]
	}
	for i := range result.Groups {
		g := &result.Groups[i]
		g.Issues, g.SearchStatus = s.GitHub.Find(ctx, g.Signature, g.Category, g.Evaluator)
		if g.Issues == nil {
			g.Issues = []Issue{}
		}
		g.Suggestion = suggestion(g.Issues, g.SearchStatus)
		g.AIStatus = "not_requested"
		if input.UseAI {
			g.AIStatus = "unavailable"
			if s.Model != nil && ctx.Err() == nil {
				facts := map[string]any{"failureCategory": g.Category, "evaluator": g.Evaluator,
					"failingSampleCount": g.FailureCount, "distinctModelRefs": len(g.Models), "samplesWithoutModelRef": g.Unattributed,
					"searchStatus": g.SearchStatus, "recommendation": g.Suggestion, "truncatedSampleScan": more}
				explanation, err := s.Model.Summarize(ctx, "Summarize a group of Echo evaluation failures and propose one next check. Do not invent sample values, model versions, root causes or issue results.", facts)
				if err == nil {
					g.AIStatus, g.AIAnalysis = "generated", explanation
				}
			}
		}
		g.Title, g.Markdown = renderDraft(run, *g, more)
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	return result, nil
}

func groupFailures(run Run, samples []Sample) []Group {
	type bucket struct {
		group  Group
		models map[string]bool
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].Index < samples[j].Index })
	buckets := make(map[string]*bucket)
	for _, sample := range samples {
		evaluator := sample.Evaluator
		if !evaluatorForm.MatchString(evaluator) {
			evaluator = "unknown"
		}
		kind := sample.category
		key := evaluator + "\x00" + kind
		b := buckets[key]
		if b == nil {
			b = &bucket{group: Group{Category: kind, Evaluator: evaluator, Signature: failureSignature(run.SuiteID, evaluator, kind), Models: []string{}, Samples: []Ref{}}, models: make(map[string]bool)}
			buckets[key] = b
		}
		b.group.FailureCount++
		model := safeText(sample.ModelRef, 200)
		if model == "" {
			b.group.Unattributed++
		} else {
			b.models[model] = true
		}
		if len(b.group.Samples) < 5 {
			b.group.Samples = append(b.group.Samples, Ref{Index: sample.Index, ID: sample.ID, InputDigest: sample.InputDigest, Score: *sample.Score, ModelRef: model})
		}
	}
	result := make([]Group, 0, len(buckets))
	for _, b := range buckets {
		for model := range b.models {
			b.group.Models = append(b.group.Models, model)
		}
		sort.Strings(b.group.Models)
		result = append(result, b.group)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].FailureCount != result[j].FailureCount {
			return result[i].FailureCount > result[j].FailureCount
		}
		return result[i].Signature < result[j].Signature
	})
	return result
}

func failureSignature(suiteID, evaluator, category string) string {
	hash := sha256.Sum256([]byte(strings.ToLower(suiteID) + "\x00" + evaluator + "\x00" + category))
	return fmt.Sprintf("cyrene-%x", hash[:10])
}

func suggestion(issues []Issue, status string) string {
	for _, issue := range issues {
		if issue.Match == "signature" && issue.State == "open" {
			return "add_evidence_to_open_match"
		}
	}
	if len(issues) > 0 {
		return "review_existing_candidates"
	}
	if status != "complete" {
		return "check_existing_issues_manually"
	}
	return "review_new_issue_draft"
}

func renderDraft(run Run, g Group, partial bool) (string, string) {
	label := searchTerm(g.Category)
	var body strings.Builder
	fmt.Fprintf(&body, "Cyrene signature: %s\n\n", g.Signature)
	fmt.Fprintf(&body, "## 评测证据\n\n- Echo run: `%s`\n- Suite: `%s`\n- Evaluator: `%s`\n- 失败类别: %s\n- 本次读取覆盖的失败样本: %d\n",
		run.ID, run.SuiteID, g.Evaluator, label, g.FailureCount)
	if partial {
		body.WriteString("- 本次只读取了部分失败样本。\n")
	}
	if len(g.Models) > 0 {
		body.WriteString("- Echo 记录的 modelRef: ")
		for i, model := range g.Models {
			if i != 0 {
				body.WriteString(", ")
			}
			fmt.Fprintf(&body, "`%s`", model)
		}
		body.WriteByte('\n')
	}
	if g.Unattributed > 0 {
		fmt.Fprintf(&body, "- 缺少 modelRef 的样本: %d\n", g.Unattributed)
	}
	body.WriteString("\n## 复核与复现条件\n\n使用有权限的 Echo 客户端打开该 run，按以下编号查阅同一输入摘要、评测器和已记录的模型引用。原始输入和输出保留在 Echo。\n\n")
	for _, sample := range g.Samples {
		fmt.Fprintf(&body, "- 样本 %d (`%s`), inputDigest `%s`, score %.6g\n", sample.Index, sample.ID, sample.InputDigest, sample.Score)
	}
	if len(g.Issues) > 0 {
		body.WriteString("\n## 已有 Issue\n\n")
		for _, issue := range g.Issues {
			fmt.Fprintf(&body, "- #%d (%s, %s) %s\n", issue.Number, issue.State, issue.Match, issue.URL)
		}
	} else if g.SearchStatus != "complete" {
		body.WriteString("\nGitHub 查重未完成，发布前请人工核对。\n")
	}
	if g.AIAnalysis != "" {
		fmt.Fprintf(&body, "\n## AI 复核建议\n\n%s\n", g.AIAnalysis)
	}
	return fmt.Sprintf("[Echo] %s (%s)", label, g.Evaluator), body.String()
}

func safeText(s string, maxRunes int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || strings.ContainsRune("`<>@", r) {
			return -1
		}
		return r
	}, s))
	if runes := []rune(s); len(runes) > maxRunes {
		s = string(runes[:maxRunes])
	}
	return s
}
