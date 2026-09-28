package fakeapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/alaindgonz-cell/intellectus/engine/internal/fakeapi"
)

func sdk(url string) anthropic.Client {
	return anthropic.NewClient(option.WithoutEnvironmentDefaults(), option.WithAPIKey("fake"), option.WithBaseURL(url), option.WithMaxRetries(0))
}

func TestAnthropicNonStreamingWithSDK(t *testing.T) {
	f := fakeapi.NewAnthropic(fakeapi.Sequence(fakeapi.AnthropicReply{
		Text: "hello", ToolName: "t", ToolInput: map[string]any{"a": 1},
		Usage: fakeapi.AnthropicUsage{InputTokens: 3, OutputTokens: 4},
	}))
	defer f.Close()
	c := sdk(f.URL())
	msg, err := c.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model: "claude-opus-5", MaxTokens: 100,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.StopReason != anthropic.StopReasonToolUse || msg.Model != "claude-opus-5" || msg.Usage.OutputTokens != 4 || len(msg.Content) != 2 {
		t.Fatalf("message %+v", msg)
	}
	if msg.Content[0].Text != "hello" || msg.Content[1].Name != "t" || string(msg.Content[1].Input) != `{"a":1}` {
		t.Fatalf("content %+v", msg.Content)
	}
	r := f.Requests()[0]
	if r.Stream || r.Model != "claude-opus-5" || r.LastMessage().Text() != "hi" || r.RawQuery != "" {
		t.Fatalf("request %+v", r)
	}
}

func TestAnthropicStreamingWithSDK(t *testing.T) {
	f := fakeapi.NewAnthropic(fakeapi.Sequence(fakeapi.AnthropicReply{
		ThinkingSignature: "sig", Text: "streamed text, longer than one chunk", ToolName: "t",
		ToolInput: map[string]any{"list": []any{"x", "y"}, "n": 2},
	}))
	defer f.Close()
	c := sdk(f.URL())
	s := c.Messages.NewStreaming(context.Background(), anthropic.MessageNewParams{
		Model: "claude-sonnet-5", MaxTokens: 100,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	msg := anthropic.Message{}
	for s.Next() {
		if err := msg.Accumulate(s.Current()); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	if len(msg.Content) != 3 || msg.Content[0].Signature != "sig" || msg.Content[1].Text != "streamed text, longer than one chunk" {
		t.Fatalf("content %+v", msg.Content)
	}
	var in map[string]any
	if err := json.Unmarshal(msg.Content[2].Input, &in); err != nil || in["n"] != float64(2) {
		t.Fatalf("tool input %s (%v)", msg.Content[2].Input, err)
	}
	if msg.StopReason != anthropic.StopReasonToolUse || msg.Model != "claude-sonnet-5" {
		t.Fatalf("stop %s model %s", msg.StopReason, msg.Model)
	}
}

func TestAnthropicErrorsAndRouting(t *testing.T) {
	f := fakeapi.NewAnthropic(fakeapi.Sequence(fakeapi.AnthropicReply{Status: 529}))
	defer f.Close()
	c := sdk(f.URL())
	_, err := c.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model: "m", MaxTokens: 1, Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("x"))},
	})
	var ae *anthropic.Error
	if !errors.As(err, &ae) || ae.StatusCode != 529 || ae.Type() != "overloaded_error" {
		t.Fatalf("err %v", err)
	}
	resp, err := http.Get(f.URL() + "/v1/other")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if n := len(f.Requests()); n != 2 {
		t.Fatalf("every request is logged, got %d", n)
	}
}

func TestAnthropicHandlerIsEmbeddable(t *testing.T) {
	h := fakeapi.NewAnthropicHandler(func(r fakeapi.AnthropicRequest) fakeapi.AnthropicReply {
		return fakeapi.AnthropicReply{Text: "seen " + r.SystemText(), Model: "claude-opus-5"}
	})
	if h.URL() != "" {
		t.Fatal("unstarted handler has no URL")
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/messages", h)
	body := `{"model":"claude-opus-5","max_tokens":5,"system":"SYS","messages":[{"role":"user","content":"q"}]}`
	rec := &recorder{header: http.Header{}}
	req, _ := http.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	mux.ServeHTTP(rec, req)
	if rec.status != 200 || !strings.Contains(rec.body.String(), `"seen SYS"`) {
		t.Fatalf("status %d body %s", rec.status, rec.body.String())
	}
	if got := h.Requests()[0].Messages[0].Text(); got != "q" {
		t.Fatalf("string content should be normalized to a text block, got %q", got)
	}
}

type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	return r.body.Write(b)
}
func (r *recorder) WriteHeader(s int) { r.status = s }

func TestJevFake(t *testing.T) {
	j := fakeapi.NewJev(func(r fakeapi.JevRequest) (int, any) {
		switch r.Seq {
		case 0:
			return 200, fakeapi.JevChoice(r.Model, "REPAIR", 0.8, nil)
		case 1:
			return 429, fakeapi.JevResponse{Header: http.Header{"Retry-After": {"2"}}, Body: []byte("slow")}
		}
		return 200, nil
	})
	defer j.Close()
	post := func(body string) (*http.Response, string) {
		resp, err := http.Post(j.URL()+"/v1/systemone", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, body := post(`{"model":"jev-1.13.0","state":"s","questions":{"route":{"type":"choice","instructions":"i","criteria":{"REPAIR":"r","STOP":"s"}}}}`)
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil || resp.StatusCode != 200 || got["model"] != "jev-1.13.0" {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	resp, body = post(`not json`)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "2" || body != "slow" {
		t.Fatalf("status %d header %v body %q", resp.StatusCode, resp.Header, body)
	}
	reqs := j.Requests()
	if reqs[0].Questions["route"].Criteria["STOP"] != "s" || reqs[0].State != "s" || reqs[0].DecodeErr != nil {
		t.Fatalf("decoded %+v", reqs[0])
	}
	if reqs[1].DecodeErr == nil {
		t.Fatal("bad JSON should be flagged")
	}
}
