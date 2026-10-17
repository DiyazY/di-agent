package autonomy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/DiyazY/di-agent/pkg/types"
)

// DecidePrompt versions the questions a decision model is asked. A change to them is
// a change to what every later decision means, so it is recorded in each.
const DecidePrompt = "review-decide-v1"

// option is one answer to a question, as every backend asks it.
type option struct{ key, text string }

// The orientation question's options, mapped back to the candidate's ends.
const (
	orientFirst   = "first_drives_second"
	orientSecond  = "second_drives_first"
	orientUnclear = "unclear"
)

const actionQuestion = "Should this candidate be recorded in the machine's model as a relationship between the two metrics?"

var actionOptions = []option{
	{types.ReviewConfirm, "Record it: the source plausibly drives, feeds or accounts for the target on this " +
		"machine, and the evidence is strong and clean enough to rely on."},
	{types.ReviewReject, "Do not record it: the correlation is most likely coincidence, both metrics following a " +
		"shared driver or overall load, or an artefact of how they are measured, so it says nothing about how " +
		"one affects the other."},
	{types.ReviewDefer, "Not yet: it may be real, but the evidence is too thin or too mixed to decide now; wait " +
		"for more readings."},
}

const orientationQuestion = "If one of the two drives the other, which one?"

func orientationOptions(c types.CandidateEdge) []option {
	return []option{
		{orientFirst, c.FromID + " drives " + c.ToID + "."},
		{orientSecond, c.ToID + " drives " + c.FromID + "."},
		{orientUnclear, "Neither can be said to drive the other from this evidence."},
	}
}

// orientation maps the orientation answer onto the candidate's ends.
func orientation(c types.CandidateEdge, p map[string]float64) map[string]float64 {
	return map[string]float64{c.FromID: p[orientFirst], c.ToID: p[orientSecond], orientUnclear: p[orientUnclear]}
}

func init() {
	RegisterDecider("systemone", func(s Spec, r Renderer) (Decider, error) {
		if s.Model == "" {
			return nil, errors.New("no model named")
		}
		timeout := s.Timeout
		if timeout <= 0 {
			timeout = 2 * time.Minute
		}
		return &SystemOne{url: strings.TrimRight(s.URL, "/"), model: s.Model, render: r,
			http: &http.Client{Timeout: timeout}}, nil
	})
}

// SystemOne is a Decider over Ollama's decision endpoint (POST /v1/systemone): a
// state and a schema of typed questions in, a probability per option out, in one
// forward pass. Decision models such as nimble-v3 and tev1 serve only this endpoint.
type SystemOne struct {
	url, model string // url is Ollama's base, without /v1
	render     Renderer
	http       *http.Client
}

func (d *SystemOne) Name() string { return d.model }

type question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

func choice(instructions string, opts []option) question {
	q := question{Type: "choice", Instructions: instructions, Criteria: map[string]string{}}
	for _, o := range opts {
		q.Criteria[o.key] = o.text
	}
	return q
}

type systemOneRequest struct {
	Model     string              `json:"model"`
	State     string              `json:"state"`
	Questions map[string]question `json:"questions"`
}

type systemOneResponse struct {
	Answers map[string]struct {
		Probabilities map[string]float64 `json:"probabilities"`
	} `json:"answers"`
	Error string `json:"error"`
}

// Decide asks whether the candidate should be confirmed, rejected or deferred and,
// when it is undirected, which end drives the other.
func (d *SystemOne) Decide(ctx context.Context, e Evidence, analysis string) (*Verdict, error) {
	c := e.Candidate
	qs := map[string]question{"action": choice(actionQuestion, actionOptions)}
	if c.Undirected {
		qs["orientation"] = choice(orientationQuestion, orientationOptions(c))
	}
	state := d.render.Render(e, analysis)
	raw, err := json.Marshal(systemOneRequest{Model: d.model, State: state, Questions: qs})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url+"/v1/systemone", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.model, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.model, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d: %s", d.model, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out systemOneResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%s: decode: %w", d.model, err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("%s: %s", d.model, out.Error)
	}
	action := out.Answers["action"].Probabilities
	if len(action) == 0 {
		return nil, fmt.Errorf("%s answered no action probabilities", d.model)
	}
	v := &Verdict{Model: d.model, Backend: "systemone", Prompt: DecidePrompt + "/" + d.render.Name(),
		State: state, Actions: action}
	if c.Undirected {
		v.Orientation = orientation(c, out.Answers["orientation"].Probabilities)
	}
	return v, nil
}
