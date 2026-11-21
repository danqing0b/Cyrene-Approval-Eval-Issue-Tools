package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"cyrene-go-plugins/internal/upstream"
)

var repoName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9_.-]{1,100}$`)
var signatureForm = regexp.MustCompile(`^cyrene-[a-f0-9]{20}$`)
var evaluatorForm = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	State  string `json:"state"`
	Match  string `json:"match"`
}

type GitHub struct {
	Client     *upstream.Client
	Repository string
}

func ValidRepository(repo string) bool {
	return repoName.MatchString(repo) && !strings.HasSuffix(repo, "/.") && !strings.HasSuffix(repo, "/..")
}

func (g GitHub) ReadIssue(ctx context.Context, number int, signature string) (*Issue, string) {
	if g.Client == nil || !ValidRepository(g.Repository) {
		return nil, "not_configured"
	}
	var row struct {
		Number      int             `json:"number"`
		Title       string          `json:"title"`
		Body        string          `json:"body"`
		HTMLURL     string          `json:"html_url"`
		State       string          `json:"state"`
		PullRequest json.RawMessage `json:"pull_request"`
	}
	path := fmt.Sprintf("repos/%s/issues/%d", g.Repository, number)
	if err := g.Client.JSON(ctx, http.MethodGet, path, nil, nil, &row, 1<<20); err != nil {
		return nil, "unavailable"
	}
	expected := fmt.Sprintf("https://github.com/%s/issues/%d", g.Repository, number)
	if row.Number != number || len(row.PullRequest) != 0 ||
		!strings.EqualFold(row.HTMLURL, expected) || row.State != "open" && row.State != "closed" {
		return nil, "invalid"
	}
	issue := &Issue{Number: number, Title: safeText(row.Title, 160), URL: expected, State: row.State, Match: "requested"}
	if signature != "" {
		if !hasSignature(row.Body, signature) {
			return issue, "signature_unconfirmed"
		}
		issue.Match = "signature"
	}
	return issue, "verified"
}

func (g GitHub) Find(ctx context.Context, signature, category, evaluator string) ([]Issue, string) {
	if g.Client == nil || !ValidRepository(g.Repository) {
		return nil, "not_configured"
	}
	if !signatureForm.MatchString(signature) {
		return nil, "unavailable"
	}
	exact, complete, err := g.search(ctx, `in:body "`+signature+`"`, signature)
	if err != nil {
		return nil, "unavailable"
	}
	for _, issue := range exact {
		if issue.Match == "signature" {
			return exact, coverage(complete)
		}
	}
	if !complete || len(exact) > 0 {
		return exact, coverage(complete)
	}
	if label := searchTerm(category); label != "" && evaluatorForm.MatchString(evaluator) && evaluator != "unknown" {
		candidates, covered, err := g.search(ctx, fmt.Sprintf(`in:title "%s" "%s"`, label, evaluator), signature)
		if err != nil {
			return exact, "partial"
		}
		return candidates, coverage(covered)
	}
	return exact, "complete"
}

func coverage(complete bool) string {
	if complete {
		return "complete"
	}
	return "partial"
}

func searchTerm(category string) string {
	switch category {
	case "missing_output":
		return "missing output"
	case "output_type_mismatch":
		return "output type mismatch"
	case "exact_match_failed":
		return "exact match failed"
	case "score_below_threshold":
		return "evaluation score below threshold"
	}
	return ""
}

func (g GitHub) search(ctx context.Context, filter, signature string) ([]Issue, bool, error) {
	query := url.Values{"q": {"repo:" + g.Repository + " is:issue " + filter}, "per_page": {"10"}}
	var response struct {
		Incomplete bool `json:"incomplete_results"`
		Total      *int `json:"total_count"`
		Items      []struct {
			Number      int            `json:"number"`
			Title       string         `json:"title"`
			Body        string         `json:"body"`
			HTMLURL     string         `json:"html_url"`
			State       string         `json:"state"`
			PullRequest map[string]any `json:"pull_request"`
		} `json:"items"`
	}
	if err := g.Client.JSON(ctx, http.MethodGet, "search/issues", query, nil, &response, 2<<20); err != nil {
		return nil, false, err
	}
	if response.Total == nil || *response.Total < len(response.Items) || response.Items == nil || len(response.Items) > 10 {
		return nil, false, errors.New("unexpected GitHub search response")
	}
	result := make([]Issue, 0, len(response.Items))
	complete := !response.Incomplete && *response.Total == len(response.Items)
	for _, row := range response.Items {
		expected := fmt.Sprintf("https://github.com/%s/issues/%d", g.Repository, row.Number)
		if row.PullRequest != nil || !strings.EqualFold(row.HTMLURL, expected) || row.Number < 1 || row.State != "open" && row.State != "closed" {
			complete = false
			continue
		}
		match := "candidate"
		if hasSignature(row.Body, signature) {
			match = "signature"
		}
		result = append(result, Issue{Number: row.Number, Title: safeText(row.Title, 160), URL: expected, State: row.State, Match: match})
	}
	return result, complete, nil
}

func hasSignature(body, signature string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "Cyrene signature: "+signature {
			return true
		}
	}
	return false
}
