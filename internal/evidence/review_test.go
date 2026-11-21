package evidence

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"cyrene-go-plugins/internal/upstream"
)

func TestReviewMatchesEvidenceAcrossChangedArtifacts(t *testing.T) {
	const beforeID = "11111111-1111-4111-8111-111111111111"
	const afterID = "22222222-2222-4222-8222-222222222222"
	const suiteID = "33333333-3333-4333-8333-333333333333"
	const beforeResultID = "44444444-4444-4444-8444-444444444444"
	const afterResultID = "55555555-5555-4555-8555-555555555555"
	const evaluator = "exact_match.v1"
	const oldModel = "model-version://old"
	const newModel = "model-version://new"
	beforeDigest := "sha256:" + strings.Repeat("a", 64)
	afterDigest := "sha256:" + strings.Repeat("b", 64)
	packageDigest := "sha256:" + strings.Repeat("c", 64)
	signature := failureSignature(suiteID, evaluator, "exact_match_failed")

	makeSample := func(run, digest, sample, reference, actual, model string, index int, passed bool) map[string]any {
		ordinal := index
		if run == afterID {
			ordinal += 100
		}
		score := 0
		if passed {
			score = 1
		}
		return map[string]any{
			"id": fmt.Sprintf("00000000-0000-4000-8000-%012d", ordinal), "runId": run,
			"sampleIndex": index, "inputDigest": digest, "evaluator": evaluator, "score": score,
			"passed": passed, "expected": reference, "actual": actual, "modelRef": model,
			"inputRecord": map[string]any{
				"sampleId": sample, "reference": reference, "actual": actual, "model": model,
			},
		}
	}
	b := func(i int, key, reference, actual string, passed bool) map[string]any {
		return makeSample(beforeID, beforeDigest, key, reference, actual, oldModel, i, passed)
	}
	a := func(i int, key, reference, actual string, passed bool) map[string]any {
		return makeSample(afterID, afterDigest, key, reference, actual, newModel, i, passed)
	}
	before := []map[string]any{
		b(1, "recovers", "private-reference", "wrong", false),
		b(2, "fails", "right", "wrong", false),
		b(3, "different-reference", "right", "wrong", false),
		b(4, "regresses", "right", "right", true),
		b(5, "missing", "right", "wrong", false),
		b(6, "duplicate", "right", "wrong", false),
	}
	after := []map[string]any{
		a(1, "fails", "right", "wrong again", false),
		a(2, "different-reference", "updated", "updated", true),
		a(3, "recovers", "private-reference", "private-reference", true),
		a(4, "regresses", "right", "wrong", false),
		a(5, "duplicate", "right", "right", true),
		a(6, "duplicate", "right", "wrong", false),
	}
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected write: %s %s", r.Method, r.URL.Path)
		}
		var reply any
		switch r.URL.Path {
		case "/api/v1/evaluation-runs/" + beforeID, "/api/v1/evaluation-runs/" + afterID:
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/evaluation-runs/")
			inputDigest, resultID := beforeDigest, beforeResultID
			if id == afterID {
				inputDigest, resultID = afterDigest, afterResultID
			}
			reply = map[string]any{"id": id, "suiteId": suiteID, "state": "SUCCEEDED", "engineBindingId": "exact",
				"resourceVersion": 2, "resultId": resultID, "inputArtifact": map[string]string{"digest": inputDigest}}
		case "/api/v1/evaluation-suites/" + suiteID:
			reply = map[string]any{"id": suiteID, "resourceVersion": 1, "evaluator": evaluator, "expectedField": "reference", "actualField": "actual"}
		case "/api/v1/evaluation-results/" + beforeResultID + "/export", "/api/v1/evaluation-results/" + afterResultID + "/export":
			id := strings.Split(r.URL.Path, "/")[4]
			inputDigest := beforeDigest
			if id == afterResultID {
				inputDigest = afterDigest
			}
			reply = map[string]any{"schemaVersion": "cyrene.echo.evaluation-report.v1", "resultId": id, "inputDigest": inputDigest,
				"target":    map[string]string{"versionRef": "dataset-version://stable", "packageDigest": packageDigest},
				"evaluator": map[string]string{"id": evaluator, "version": "1"}}
		case "/api/v1/evaluation-runs/" + beforeID + "/samples", "/api/v1/evaluation-runs/" + afterID + "/samples":
			rows := before
			if strings.Contains(r.URL.Path, afterID) {
				rows = after
			}
			if r.URL.Query().Has("only_passed") {
				t.Error("review needs both passed and failed samples")
			}
			offset, offsetErr := strconv.Atoi(r.URL.Query().Get("offset"))
			limit, limitErr := strconv.Atoi(r.URL.Query().Get("limit"))
			if offsetErr != nil || limitErr != nil || offset < 0 || offset > len(rows) || limit < 1 {
				t.Errorf("unexpected page: %s", r.URL.RawQuery)
				http.Error(w, "bad pagination", http.StatusBadRequest)
				return
			}
			reply = rows[offset:min(offset+limit, len(rows))]
		case "/repos/do/example/issues/3":
			reply = map[string]any{"number": 3, "title": "Original issue", "state": "open", "html_url": "https://github.com/do/example/issues/3", "body": "Cyrene signature: " + signature}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(reply); err != nil {
			t.Error(err)
		}
	}))
	defer host.Close()
	client, err := upstream.New(host.URL, "test-read")
	if err != nil {
		t.Fatal(err)
	}
	service := Service{Echo: Echo{Client: client}, GitHub: GitHub{Client: client, Repository: "do/example"}}
	issueNumber := 3
	input := ReviewInput{BaselineRunID: beforeID, CandidateRunID: afterID, IssueNumber: &issueNumber, CandidateModelRef: newModel}
	got, err := service.Review(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	want := ReviewCounts{Recovered: 1, StillFailing: 1, Regressed: 1, NotComparable: 3}
	if got.Counts != want || got.InputEvidence != "same_dataset_target_and_sample_input" || got.IssuesStatus != "verified" {
		t.Fatalf("review: counts %+v (want %+v), evidence %s, issue %s", got.Counts, want, got.InputEvidence, got.IssuesStatus)
	}
	if got.Samples[0].Candidate == nil || got.Samples[0].Candidate.Index != 3 {
		t.Fatal("sample should match its stable ID across reordered output")
	}
	reasons := []string{got.Samples[2].Reason, got.Samples[4].Reason, got.Samples[5].Reason}
	if !slices.Equal(reasons, []string{"test_input_or_reference_changed", "candidate_missing_or_skipped", "ambiguous_sample_id"}) {
		t.Fatalf("unexpected reasons %v", reasons)
	}
	if strings.Contains(got.Markdown, "private-reference") || strings.Contains(got.Markdown, "wrong again") {
		t.Fatal("review reproduces raw evaluation data")
	}
	input.GroupSignature = signature
	group, err := service.Review(context.Background(), input)
	if err != nil || group.Issue == nil || group.Issue.Match != "signature" || group.Counts.Regressed != 0 {
		t.Fatalf("scoped review: %+v %v", group, err)
	}
	input.GroupSignature = ""
	input.CandidateModelRef = "wrong-model"
	maxSamples := 1
	input.MaxSamples = &maxSamples
	limited, err := service.Review(context.Background(), input)
	if err != nil || !limited.Baseline.Truncated || !limited.Candidate.Truncated || limited.Samples[0].Reason != "candidate_scan_incomplete" {
		t.Fatalf("limited review: %+v %v", limited, err)
	}
	input.MaxSamples = nil
	wrongModel, err := service.Review(context.Background(), input)
	if err != nil || wrongModel.Counts.Recovered != 0 || wrongModel.Samples[0].Reason != "candidate_model_ref_mismatch" {
		t.Fatalf("model version validation: %+v %v", wrongModel, err)
	}
}
