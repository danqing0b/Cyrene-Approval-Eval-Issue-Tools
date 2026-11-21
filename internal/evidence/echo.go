package evidence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"cyrene-go-plugins/internal/upstream"
)

var uuid = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type Run struct {
	ID        string `json:"id"`
	SuiteID   string `json:"suiteId"`
	State     string `json:"state"`
	Revision  int64  `json:"resourceVersion"`
	BindingID string `json:"engineBindingId"`
	ResultID  string `json:"resultId"`
	Artifact  struct {
		Digest string `json:"digest"`
	} `json:"inputArtifact"`
}

type Sample struct {
	ID          string          `json:"id"`
	RunID       string          `json:"runId"`
	Index       int             `json:"sampleIndex"`
	Evaluator   string          `json:"evaluator"`
	InputDigest string          `json:"inputDigest"`
	InputRecord json.RawMessage `json:"inputRecord"`
	Expected    json.RawMessage `json:"expected"`
	Actual      json.RawMessage `json:"actual"`
	Passed      *bool           `json:"passed"`
	Score       *float64        `json:"score"`
	ModelRef    string          `json:"modelRef"`
	Judge       string          `json:"judgeIdentity"`
	category    string
	caseID      string
	caseDigest  string
	hasInput    bool
}

type Suite struct {
	ID            string `json:"id"`
	Revision      int64  `json:"resourceVersion"`
	Evaluator     string `json:"evaluator"`
	ExpectedField string `json:"expectedField"`
	ActualField   string `json:"actualField"`
	JudgeProfile  string `json:"judgeProfileId"`
}

type Echo struct {
	Client       *upstream.Client
	WorkspaceAPI bool
}

func (e Echo) apiPath(resource string) string {
	if e.WorkspaceAPI {
		return "internal/workspace/v1/" + resource
	}
	return "api/v1/" + resource
}

func (e Echo) ReadFailures(ctx context.Context, runID string, limit int) (Run, []Sample, bool, error) {
	run, err := e.completedRun(ctx, runID)
	if err != nil {
		return Run{}, nil, false, err
	}
	samples, more, err := e.readSamples(ctx, run, limit, true, nil)
	return run, samples, more, err
}

func (e Echo) completedRun(ctx context.Context, runID string) (Run, error) {
	if e.Client == nil {
		return Run{}, errors.New("Echo read service is not configured")
	}
	var run Run
	if err := e.Client.JSON(ctx, http.MethodGet, e.apiPath("evaluation-runs/"+runID), nil, nil, &run, 128<<10); err != nil {
		return Run{}, err
	}
	if !strings.EqualFold(run.ID, runID) || !uuid.MatchString(run.SuiteID) || run.Revision < 1 {
		return Run{}, errors.New("Echo run receipt does not match the requested evaluation")
	}
	if run.State != "SUCCEEDED" {
		return Run{}, fmt.Errorf("Echo run %s is %q; sample inspection requires a completed evaluation", run.ID, run.State)
	}
	return run, nil
}

func (e Echo) suite(ctx context.Context, id string) (Suite, error) {
	var suite Suite
	if err := e.Client.JSON(ctx, http.MethodGet, e.apiPath("evaluation-suites/"+id), nil, nil, &suite, 128<<10); err != nil {
		return Suite{}, err
	}
	if !strings.EqualFold(suite.ID, id) || suite.Revision < 1 ||
		suite.ExpectedField == "" || suite.ActualField == "" || suite.ExpectedField == suite.ActualField ||
		suite.Evaluator != "exact_match.v1" && (suite.Evaluator != "llm_judge.v1" || !uuid.MatchString(suite.JudgeProfile)) {
		return Suite{}, errors.New("Echo returned a suite without verifiable comparison settings")
	}
	return suite, nil
}

type datasetTarget struct {
	Version string `json:"versionRef"`
	Digest  string `json:"packageDigest"`
}

func (e Echo) reportTarget(ctx context.Context, run Run, suite Suite) (datasetTarget, string) {
	if !uuid.MatchString(run.ResultID) {
		return datasetTarget{}, "unavailable"
	}
	var report struct {
		Schema      string        `json:"schemaVersion"`
		ResultID    string        `json:"resultId"`
		InputDigest string        `json:"inputDigest"`
		Target      datasetTarget `json:"target"`
		Evaluator   struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		} `json:"evaluator"`
	}
	path := e.apiPath("evaluation-results/" + run.ResultID + "/export")
	if err := e.Client.JSON(ctx, http.MethodGet, path, nil, nil, &report, 8<<20); err != nil ||
		report.Schema != "cyrene.echo.evaluation-report.v1" {
		return datasetTarget{}, "unavailable"
	}
	if !strings.EqualFold(report.ResultID, run.ResultID) || report.InputDigest != run.Artifact.Digest ||
		report.Evaluator.ID != suite.Evaluator || report.Evaluator.Version != "1" ||
		report.Target.Version == "" || !digest.MatchString(report.Target.Digest) {
		return datasetTarget{}, "invalid"
	}
	return report.Target, "verified"
}

func (e Echo) readSamples(ctx context.Context, run Run, limit int, failuresOnly bool, project func(*Sample)) ([]Sample, bool, error) {
	path := e.apiPath("evaluation-runs/" + run.ID)
	const pageSize = 30
	var samples []Sample
	indexes := make(map[int]bool)
	ids := make(map[string]bool)
	for len(samples) <= limit {
		count := min(limit+1-len(samples), pageSize)
		query := url.Values{"limit": {strconv.Itoa(count)}, "offset": {strconv.Itoa(len(samples))}}
		if failuresOnly {
			query.Set("only_passed", "false")
		}
		var page []Sample
		if err := e.Client.JSON(ctx, http.MethodGet, path+"/samples", query, nil, &page, 8<<20); err != nil {
			return nil, false, err
		}
		if page == nil || len(page) > count {
			return nil, false, errors.New("Echo returned an invalid sample page")
		}
		for _, sample := range page {
			if !uuid.MatchString(sample.ID) || !strings.EqualFold(sample.RunID, run.ID) || sample.Index < 1 ||
				ids[strings.ToLower(sample.ID)] || indexes[sample.Index] || sample.Passed == nil || failuresOnly && *sample.Passed ||
				sample.Score == nil || *sample.Score < 0 || *sample.Score > 1 || sample.Evaluator == "" ||
				!digest.MatchString(sample.InputDigest) {
				return nil, false, errors.New("Echo returned an inconsistent sample record")
			}
			ids[strings.ToLower(sample.ID)], indexes[sample.Index] = true, true
			if !*sample.Passed {
				sample.category = category(sample)
			}
			if project != nil {
				project(&sample)
			}
			sample.Actual, sample.Expected, sample.InputRecord = nil, nil, nil
			samples = append(samples, sample)
		}
		if len(page) < count {
			break
		}
	}
	current, err := e.completedRun(ctx, run.ID)
	if err != nil {
		return nil, false, err
	}
	if current != run {
		return nil, false, errors.New("evaluation changed during read; retry with a fresh snapshot")
	}
	more := len(samples) > limit
	if more {
		samples = samples[:limit]
	}
	return samples, more, nil
}

func category(sample Sample) string {
	actual := bytes.TrimSpace(sample.Actual)
	expected := bytes.TrimSpace(sample.Expected)
	if len(actual) == 0 || bytes.Equal(actual, []byte("null")) || blankString(actual) {
		return "missing_output"
	}
	if len(expected) != 0 && !bytes.Equal(expected, []byte("null")) && jsonKind(actual) != jsonKind(expected) {
		return "output_type_mismatch"
	}
	if sample.Evaluator == "exact_match.v1" {
		return "exact_match_failed"
	}
	return "score_below_threshold"
}

func blankString(raw []byte) bool {
	var text string
	return json.Unmarshal(raw, &text) == nil && strings.TrimSpace(text) == ""
}

func jsonKind(raw []byte) byte {
	switch raw[0] {
	case 't', 'f':
		return 'b'
	case '{', '[', '"', 'n':
		return raw[0]
	default:
		return '#'
	}
}
