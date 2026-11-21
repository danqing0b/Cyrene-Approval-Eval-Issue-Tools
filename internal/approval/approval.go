package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"cyrene-go-plugins/internal/upstream"
)

const maxChanges = 100

var identity = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,199}$`)

type Target struct {
	Kind string `json:"kind" jsonschema:"Resource type, e.g. pipeline or gateway-route."`
	ID   string `json:"id" jsonschema:"Stable target identifier supplied by the host."`
}

type Snapshot struct {
	Revision *int64         `json:"revision" jsonschema:"Current or proposed revision of the same target. Use 0 for a new resource."`
	Params   map[string]any `json:"params" jsonschema:"Host-projected parameter snapshot. Include only relevant non-secret fields."`
}

type Input struct {
	Kind             string   `json:"kind" jsonschema:"Navigator approval kind, e.g. pipelines.patch."`
	WorkspaceID      string   `json:"workspaceId" jsonschema:"Workspace recorded by the host."`
	PipelineID       string   `json:"pipelineId,omitempty" jsonschema:"Owning pipeline ID when relevant."`
	Target           Target   `json:"target"`
	ExpectedRevision *int64   `json:"expectedRevision" jsonschema:"Revision recorded in the operation preview."`
	Before           Snapshot `json:"before"`
	After            Snapshot `json:"after"`
	UseAI            bool     `json:"useAI,omitempty" jsonschema:"Request an optional model explanation of the computed diff."`
}

type Change struct {
	Path   string          `json:"path"`
	Op     string          `json:"op"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
}

type Details struct {
	WorkspaceID    string   `json:"workspaceId"`
	PipelineID     string   `json:"pipelineId,omitempty"`
	Target         Target   `json:"target"`
	BeforeRevision int64    `json:"beforeRevision"`
	AfterRevision  int64    `json:"afterRevision"`
	Changes        []Change `json:"changes"`
	AIExplanation  string   `json:"aiExplanation,omitempty"`
	AIStatus       string   `json:"aiStatus"`
}

type Result struct {
	Kind    string  `json:"kind"`
	Summary string  `json:"summary"`
	Details Details `json:"details"`
}

func Explain(ctx context.Context, input Input, narrator upstream.Narrator) (Result, error) {
	for _, id := range []string{input.Kind, input.WorkspaceID, input.Target.Kind, input.Target.ID} {
		if !identity.MatchString(id) {
			return Result{}, errors.New("kind, workspace and target must use stable identifiers")
		}
	}
	if len(input.Kind) > 128 || input.PipelineID != "" && !identity.MatchString(input.PipelineID) {
		return Result{}, errors.New("invalid approval kind or pipeline identifier")
	}
	if input.ExpectedRevision == nil || input.Before.Revision == nil || input.After.Revision == nil || input.Before.Params == nil || input.After.Params == nil {
		return Result{}, errors.New("provide both parameter snapshots, both revisions and the preview revision")
	}
	before, after := *input.Before.Revision, *input.After.Revision
	if before < 0 || after < before || before > 9007199254740991 || after > 9007199254740991 || *input.ExpectedRevision != before {
		return Result{}, errors.New("revision mismatch: reread the target and regenerate the preview")
	}
	for _, params := range []map[string]any{input.Before.Params, input.After.Params} {
		raw, err := json.Marshal(params)
		if err != nil || len(raw) > 32<<10 || !withinLimits(params, 0) {
			return Result{}, errors.New("parameter snapshot exceeds the size, depth or field-name limit")
		}
	}
	changes := make([]Change, 0)
	diff("", input.Before.Params, true, input.After.Params, true, &changes)
	if len(changes) > maxChanges {
		return Result{}, errors.New("more than 100 changes; submit a narrower operation preview")
	}

	result := Result{Kind: input.Kind, Details: Details{
		WorkspaceID: input.WorkspaceID, PipelineID: input.PipelineID, Target: input.Target,
		BeforeRevision: before, AfterRevision: after, Changes: changes, AIStatus: "not_requested",
	}}
	result.Summary = fmt.Sprintf("%s 将作用于 %s %s（修订 %d → %d），参数差异 %d 项。",
		input.Kind, input.Target.Kind, input.Target.ID, before, after, len(changes))
	if input.UseAI {
		result.Details.AIStatus = "unavailable"
		if narrator != nil && len(changes) > 0 {
			facts := make([]map[string]string, 0, len(changes))
			for _, c := range changes {
				facts = append(facts, map[string]string{"field": c.Path, "change": c.Op})
			}
			description, err := narrator.Summarize(ctx, "Explain an approval preview. Describe the changed fields and direction. Values are intentionally withheld; never infer any values, permission, global effect, or approval outcome.", facts)
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			if err == nil {
				result.Details.AIStatus, result.Details.AIExplanation = "generated", description
				result.Summary += " AI 解读：" + description
			}
		}
		if len(changes) == 0 {
			result.Details.AIStatus = "no_changes"
		}
	}
	return result, nil
}

func withinLimits(value any, depth int) bool {
	if depth > 12 {
		return false
	}
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if key == "" || utf8.RuneCountInString(key) > 160 || strings.IndexFunc(key, unicode.IsControl) >= 0 || !withinLimits(child, depth+1) {
				return false
			}
		}
	case []any:
		for _, child := range v {
			if !withinLimits(child, depth+1) {
				return false
			}
		}
	}
	return true
}

func diff(path string, before any, hasBefore bool, after any, hasAfter bool, changes *[]Change) {
	if len(*changes) > maxChanges {
		return
	}
	b, bObject := before.(map[string]any)
	a, aObject := after.(map[string]any)
	if bObject && aObject {
		keys := make(map[string]bool, len(b)+len(a))
		for key := range b {
			keys[key] = true
		}
		for key := range a {
			keys[key] = true
		}
		names := make([]string, 0, len(keys))
		for key := range keys {
			names = append(names, key)
		}
		sort.Strings(names)
		for _, key := range names {
			old, oldOK := b[key]
			next, nextOK := a[key]
			pointer := path + "/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(key)
			if secretKey(key) {
				if oldOK != nextOK || !equal(old, next) {
					if len(*changes) <= maxChanges {
						*changes = append(*changes, Change{Path: pointer, Op: operation(oldOK, nextOK), Before: concealed(oldOK), After: concealed(nextOK)})
					}
				}
				continue
			}
			diff(pointer, old, oldOK, next, nextOK, changes)
		}
		return
	}
	if hasBefore == hasAfter && equal(before, after) {
		return
	}
	c := Change{Path: path, Op: operation(hasBefore, hasAfter)}
	if hasBefore {
		c.Before, _ = json.Marshal(redact(before))
	}
	if hasAfter {
		c.After, _ = json.Marshal(redact(after))
	}
	*changes = append(*changes, c)
}

func operation(before, after bool) string {
	if !before {
		return "add"
	}
	if !after {
		return "remove"
	}
	return "replace"
}

func concealed(present bool) json.RawMessage {
	if present {
		return json.RawMessage(`"[redacted]"`)
	}
	return nil
}

func secretKey(key string) bool {
	key = strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(key))
	for _, term := range []string{"password", "apikey", "authorization", "credential", "secret", "privatekey", "accesstoken", "refreshtoken", "bearer", "cookie", "token"} {
		if key == term || strings.HasSuffix(key, term) {
			return true
		}
	}
	return false
}

func redact(value any) any {
	switch v := value.(type) {
	case map[string]any:
		safe := make(map[string]any, len(v))
		for key, child := range v {
			if secretKey(key) {
				safe[key] = "[redacted]"
			} else {
				safe[key] = redact(child)
			}
		}
		return safe
	case []any:
		safe := make([]any, len(v))
		for i, child := range v {
			safe[i] = redact(child)
		}
		return safe
	case string:
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "bearer ") || strings.Contains(v, "-----BEGIN PRIVATE KEY-----") {
			return "[redacted]"
		}
	}
	return value
}

func equal(before, after any) bool {
	switch b := before.(type) {
	case map[string]any:
		a, ok := after.(map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for key, value := range b {
			if other, ok := a[key]; !ok || !equal(value, other) {
				return false
			}
		}
		return true
	case []any:
		a, ok := after.([]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i := range b {
			if !equal(b[i], a[i]) {
				return false
			}
		}
		return true
	case json.Number:
		a, ok := after.(json.Number)
		if !ok || a == b {
			return ok
		}
		aRat, aOK := boundedNumber(a)
		bRat, bOK := boundedNumber(b)
		return aOK && bOK && aRat.Cmp(bRat) == 0
	default:
		return reflect.DeepEqual(before, after)
	}
}

func boundedNumber(value json.Number) (*big.Rat, bool) {
	text := value.String()
	if len(text) > 128 {
		return nil, false
	}
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		exponent, err := strconv.Atoi(text[i+1:])
		if err != nil || exponent < -1000 || exponent > 1000 {
			return nil, false
		}
	}
	return new(big.Rat).SetString(text)
}

// Preserve JSON numbers exactly during MCP and CLI decoding, including integers
// greater than 2^53 in parameter payloads.
func (s *Snapshot) UnmarshalJSON(raw []byte) error {
	type plain Snapshot
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	var value plain
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	*s = Snapshot(value)
	return nil
}
