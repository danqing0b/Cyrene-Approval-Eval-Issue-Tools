package evidence

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type ReviewInput struct {
	BaselineRunID     string `json:"baselineRunId"`
	CandidateRunID    string `json:"candidateRunId"`
	IssueNumber       *int   `json:"issueNumber,omitempty"`
	GroupSignature    string `json:"groupSignature,omitempty"`
	CandidateModelRef string `json:"candidateModelRef,omitempty"`
	MaxSamples        *int   `json:"maxSamples,omitempty"`
	UseAI             bool   `json:"useAI,omitempty"`
}

type ReviewRun struct {
	ID        string `json:"id"`
	SuiteID   string `json:"suiteId"`
	Revision  int64  `json:"revision"`
	Scanned   int    `json:"scanned"`
	Truncated bool   `json:"truncated"`
}

type ReviewCounts struct {
	Recovered     int `json:"recovered"`
	StillFailing  int `json:"stillFailing"`
	Regressed     int `json:"regressed"`
	UnchangedPass int `json:"unchangedPass"`
	NotComparable int `json:"notComparable"`
}

type ReviewSample struct {
	Outcome   string `json:"outcome"`
	Reason    string `json:"reason,omitempty"`
	Baseline  Ref    `json:"baseline"`
	Candidate *Ref   `json:"candidate,omitempty"`
}

type ReviewResult struct {
	Baseline          ReviewRun      `json:"baseline"`
	Candidate         ReviewRun      `json:"candidate"`
	SuiteRevision     int64          `json:"suiteRevision,omitempty"`
	GroupSignature    string         `json:"groupSignature,omitempty"`
	CandidateModelRef string         `json:"expectedCandidateModelRef,omitempty"`
	InputEvidence     string         `json:"inputEvidence"`
	Counts            ReviewCounts   `json:"counts"`
	Samples           []ReviewSample `json:"samples"`
	IssuesStatus      string         `json:"issueLookupStatus"`
	IssueNumber       *int           `json:"requestedIssueNumber,omitempty"`
	Issue             *Issue         `json:"issue,omitempty"`
	Repository        string         `json:"repository,omitempty"`
	AIStatus          string         `json:"aiStatus"`
	AIAnalysis        string         `json:"aiAnalysis,omitempty"`
	Markdown          string         `json:"draftMarkdown"`
}

func (s Service) Review(ctx context.Context, input ReviewInput) (ReviewResult, error) {
	if !uuid.MatchString(input.BaselineRunID) || !uuid.MatchString(input.CandidateRunID) ||
		strings.EqualFold(input.BaselineRunID, input.CandidateRunID) {
		return ReviewResult{}, errors.New("provide two distinct Echo evaluation run UUIDs")
	}
	if input.IssueNumber != nil && (*input.IssueNumber < 1 || *input.IssueNumber > 2147483647) {
		return ReviewResult{}, errors.New("issueNumber must be between 1 and 2147483647")
	}
	if input.GroupSignature != "" && !signatureForm.MatchString(input.GroupSignature) {
		return ReviewResult{}, errors.New("groupSignature must match a signature from cyrene_draft_eval_issues")
	}
	if len(input.CandidateModelRef) > 200 || strings.TrimSpace(input.CandidateModelRef) != input.CandidateModelRef ||
		strings.IndexFunc(input.CandidateModelRef, unicode.IsControl) >= 0 {
		return ReviewResult{}, errors.New("candidateModelRef is invalid")
	}
	limit := 300
	if input.MaxSamples != nil {
		limit = *input.MaxSamples
	}
	if limit < 1 || limit > 300 {
		return ReviewResult{}, errors.New("maxSamples must be between 1 and 300 per run")
	}

	ctx, cancel := context.WithTimeout(ctx, 100*time.Second)
	defer cancel()
	baseline, err := s.Echo.completedRun(ctx, strings.ToLower(input.BaselineRunID))
	if err != nil {
		return ReviewResult{}, err
	}
	candidate, err := s.Echo.completedRun(ctx, strings.ToLower(input.CandidateRunID))
	if err != nil {
		return ReviewResult{}, err
	}
	policy, err := s.comparisonPolicy(ctx, baseline, candidate)
	if err != nil {
		return ReviewResult{}, err
	}

	var project func(*Sample)
	if policy.reason == "" && !policy.sameArtifact {
		project = func(sample *Sample) { projectCase(sample, policy.suite) }
	}
	before, beforeMore, err := s.Echo.readSamples(ctx, baseline, limit, false, project)
	if err != nil {
		return ReviewResult{}, err
	}
	after, afterMore, err := s.Echo.readSamples(ctx, candidate, limit, false, project)
	if err != nil {
		return ReviewResult{}, err
	}
	result := ReviewResult{
		Baseline: reviewRun(baseline, len(before), beforeMore), Candidate: reviewRun(candidate, len(after), afterMore),
		SuiteRevision: policy.suite.Revision, GroupSignature: input.GroupSignature, CandidateModelRef: safeText(input.CandidateModelRef, 200),
		InputEvidence: policy.evidence, Samples: []ReviewSample{}, IssueNumber: input.IssueNumber,
		IssuesStatus: "not_requested", Repository: s.GitHub.Repository, AIStatus: "not_requested",
	}
	result.compare(before, after, baseline, candidate, policy, input)

	if input.IssueNumber != nil {
		result.Issue, result.IssuesStatus = s.GitHub.ReadIssue(ctx, *input.IssueNumber, input.GroupSignature)
	}
	if input.UseAI {
		result.AIStatus = "unavailable"
		if s.Model != nil {
			facts := map[string]any{"counts": result.Counts, "inputEvidence": result.InputEvidence,
				"baselineTruncated": beforeMore, "candidateTruncated": afterMore, "issueLookupStatus": result.IssuesStatus}
			explanation, err := s.Model.Summarize(ctx, "Explain the sampled evaluation comparison and its coverage. Never declare an issue fixed, diagnose a root cause, or assume unseen samples passed.", facts)
			if err == nil {
				result.AIStatus, result.AIAnalysis = "generated", explanation
			}
		}
	}
	if ctx.Err() != nil {
		return ReviewResult{}, ctx.Err()
	}
	result.Markdown = renderReview(result)
	return result, nil
}

type comparisonPolicy struct {
	suite        Suite
	sameArtifact bool
	sameTarget   bool
	evidence     string
	reason       string
}

func (s Service) comparisonPolicy(ctx context.Context, baseline, candidate Run) (comparisonPolicy, error) {
	switch {
	case !strings.EqualFold(baseline.SuiteID, candidate.SuiteID):
		return comparisonPolicy{evidence: "incompatible", reason: "suite_changed"}, nil
	case baseline.BindingID == "" || baseline.BindingID != candidate.BindingID:
		return comparisonPolicy{evidence: "incompatible", reason: "engine_binding_changed_or_missing"}, nil
	case !digest.MatchString(baseline.Artifact.Digest) || !digest.MatchString(candidate.Artifact.Digest):
		return comparisonPolicy{evidence: "incompatible", reason: "input_artifact_digest_missing"}, nil
	}
	suite, err := s.Echo.suite(ctx, baseline.SuiteID)
	if err != nil {
		return comparisonPolicy{}, err
	}
	policy := comparisonPolicy{suite: suite}
	if baseline.Artifact.Digest == candidate.Artifact.Digest {
		policy.sameArtifact, policy.evidence = true, "same_input_artifact"
		return policy, nil
	}
	beforeTarget, beforeStatus := s.Echo.reportTarget(ctx, baseline, suite)
	afterTarget, afterStatus := s.Echo.reportTarget(ctx, candidate, suite)
	switch {
	case beforeStatus == "invalid" || afterStatus == "invalid":
		policy.evidence, policy.reason = "incompatible", "invalid_target_report"
	case beforeStatus == "verified" && afterStatus == "verified":
		if beforeTarget != afterTarget {
			policy.evidence, policy.reason = "incompatible", "dataset_target_changed"
		} else {
			policy.sameTarget, policy.evidence = true, "same_dataset_target_and_sample_input"
		}
	case beforeStatus == "verified" || afterStatus == "verified":
		policy.evidence, policy.reason = "incompatible", "dataset_target_unconfirmed"
	default:
		policy.evidence = "sample_id_and_recorded_input"
	}
	return policy, nil
}

func reviewRun(run Run, scanned int, truncated bool) ReviewRun {
	return ReviewRun{ID: run.ID, SuiteID: run.SuiteID, Revision: run.Revision, Scanned: scanned, Truncated: truncated}
}

func (r *ReviewResult) compare(before, after []Sample, baseline, candidate Run, policy comparisonPolicy, input ReviewInput) {
	key := func(sample Sample) string {
		if policy.sameArtifact {
			return strconv.Itoa(sample.Index)
		}
		return sample.caseID
	}
	beforeIndex, afterIndex := samplesByKey(before, key), samplesByKey(after, key)
	sort.Slice(before, func(i, j int) bool { return before[i].Index < before[j].Index })
	for _, sample := range before {
		if input.GroupSignature != "" && (*sample.Passed ||
			failureSignature(baseline.SuiteID, evaluatorName(sample.Evaluator), sample.category) != input.GroupSignature) {
			continue
		}
		row := ReviewSample{Baseline: reviewRef(sample)}
		k := key(sample)
		matches := afterIndex[k]
		if k != "" && len(matches) == 1 && len(beforeIndex[k]) == 1 && policy.reason == "" {
			ref := reviewRef(matches[0])
			row.Candidate = &ref
		}
		row.Reason = policy.reason
		if row.Reason == "" {
			switch {
			case k == "":
				row.Reason = "stable_sample_id_missing"
			case len(beforeIndex[k]) > 1 || len(matches) > 1:
				row.Reason = "ambiguous_sample_id"
			case len(matches) == 0 && r.Candidate.Truncated:
				row.Reason = "candidate_scan_incomplete"
			case len(matches) == 0:
				row.Reason = "candidate_missing_or_skipped"
			case !policy.sameArtifact && !policy.sameTarget && (r.Baseline.Truncated || r.Candidate.Truncated):
				row.Reason = "sample_id_uniqueness_unconfirmed"
			default:
				row.Reason = checkPair(sample, matches[0], baseline, candidate, policy, input.CandidateModelRef)
			}
		}
		switch {
		case row.Reason != "":
			row.Outcome = "not_comparable"
			r.Counts.NotComparable++
		case !*sample.Passed && *matches[0].Passed:
			row.Outcome = "recovered"
			r.Counts.Recovered++
		case !*sample.Passed:
			row.Outcome = "still_failing"
			r.Counts.StillFailing++
		case !*matches[0].Passed:
			row.Outcome = "regressed"
			r.Counts.Regressed++
		default:
			r.Counts.UnchangedPass++
			continue
		}
		r.Samples = append(r.Samples, row)
	}
}

func samplesByKey(samples []Sample, key func(Sample) string) map[string][]Sample {
	grouped := make(map[string][]Sample, len(samples))
	for _, sample := range samples {
		if k := key(sample); k != "" {
			grouped[k] = append(grouped[k], sample)
		}
	}
	return grouped
}

func checkPair(before, after Sample, baseline, candidate Run, policy comparisonPolicy, expectedModel string) string {
	switch {
	case before.InputDigest != baseline.Artifact.Digest || after.InputDigest != candidate.Artifact.Digest:
		return "sample_artifact_mismatch"
	case before.Evaluator != policy.suite.Evaluator || after.Evaluator != policy.suite.Evaluator:
		return "evaluator_changed"
	case policy.suite.Evaluator == "llm_judge.v1" && (before.Judge == "" || before.Judge != after.Judge):
		return "judge_changed_or_missing"
	case expectedModel != "" && after.ModelRef != expectedModel:
		return "candidate_model_ref_mismatch"
	case !policy.sameArtifact && !policy.sameTarget && (!before.hasInput || !after.hasInput):
		return "recorded_test_input_missing"
	case !policy.sameArtifact && (before.caseDigest == "" || after.caseDigest == ""):
		return "reference_or_input_projection_missing"
	case !policy.sameArtifact && before.caseDigest != after.caseDigest:
		return "test_input_or_reference_changed"
	default:
		return ""
	}
}

func evaluatorName(value string) string {
	if evaluatorForm.MatchString(value) {
		return value
	}
	return "unknown"
}

func reviewRef(sample Sample) Ref {
	return Ref{Index: sample.Index, ID: sample.ID, InputDigest: sample.InputDigest, Score: *sample.Score, ModelRef: safeText(sample.ModelRef, 200)}
}

func renderReview(result ReviewResult) string {
	var body strings.Builder
	body.WriteString("## Echo 修复复核\n\n")
	if result.GroupSignature != "" {
		fmt.Fprintf(&body, "分组指纹：`%s`\n\n", result.GroupSignature)
	}
	if result.Issue != nil {
		fmt.Fprintf(&body, "关联工单：%s · %s\n\n", result.Issue.URL, result.IssuesStatus)
	} else if result.IssueNumber != nil {
		fmt.Fprintf(&body, "指定工单：#%d · %s\n\n", *result.IssueNumber, result.IssuesStatus)
	}
	fmt.Fprintf(&body, "基线：`%s`（读取 %d 条）\n\n候选：`%s`（读取 %d 条）\n\n", result.Baseline.ID, result.Baseline.Scanned, result.Candidate.ID, result.Candidate.Scanned)
	fmt.Fprintf(&body, "输入核对：`%s`\n\n", result.InputEvidence)
	if result.InputEvidence == "same_input_artifact" {
		body.WriteString("两次评测使用同一份制品，结果仅说明对现有输出的复评分数。\n\n")
	}
	if result.CandidateModelRef != "" {
		fmt.Fprintf(&body, "指定候选模型引用：`%s`\n\n", result.CandidateModelRef)
	}
	if result.Baseline.Truncated || result.Candidate.Truncated {
		body.WriteString("至少一个 run 达到样本预算，结论只覆盖已读取记录。\n\n")
	}
	c := result.Counts
	fmt.Fprintf(&body, "恢复 %d · 仍失败 %d · 新增失败 %d · 仍通过 %d · 无法比较 %d\n",
		c.Recovered, c.StillFailing, c.Regressed, c.UnchangedPass, c.NotComparable)
	if len(result.Samples) > 0 {
		body.WriteString("\n### 样本证据\n\n")
		for _, row := range result.Samples[:min(30, len(result.Samples))] {
			fmt.Fprintf(&body, "- %s：基线样本 %d (`%s`)", reviewOutcomeLabels[row.Outcome], row.Baseline.Index, row.Baseline.ID)
			if row.Candidate != nil {
				fmt.Fprintf(&body, " → 候选样本 %d (`%s`)，分数 %.6g → %.6g", row.Candidate.Index, row.Candidate.ID, row.Baseline.Score, row.Candidate.Score)
				fmt.Fprintf(&body, "，模型引用 `%s` → `%s`", modelEvidence(row.Baseline.ModelRef), modelEvidence(row.Candidate.ModelRef))
			}
			if row.Reason != "" {
				fmt.Fprintf(&body, "（`%s`）", row.Reason)
			}
			body.WriteByte('\n')
		}
		if omitted := len(result.Samples) - 30; omitted > 0 {
			fmt.Fprintf(&body, "\n另有 %d 条样本证据见结构化结果。\n", omitted)
		}
	} else if c.UnchangedPass == 0 {
		body.WriteString("\n当前读取范围内没有符合条件的基线样本。\n")
	}
	if result.AIAnalysis != "" {
		fmt.Fprintf(&body, "\n### AI 复核说明\n\n%s\n", result.AIAnalysis)
	}
	return body.String()
}

var reviewOutcomeLabels = map[string]string{
	"recovered": "恢复", "still_failing": "仍失败", "regressed": "新增失败", "not_comparable": "无法比较",
}

func modelEvidence(ref string) string {
	if ref == "" {
		return "Echo 未记录"
	}
	return ref
}
