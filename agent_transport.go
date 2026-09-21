package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const agentSSEKeepaliveInterval = 15 * time.Second

func (a *app) callAgentPlanner(ctx context.Context, model string, messages []chatMessage, tools []toolDefinition) (chatAPIResponse, error) {
	return a.callAgentPlannerWithKeepalive(ctx, model, messages, tools, nil)
}

func (a *app) callAgentPlannerWithKeepalive(ctx context.Context, model string, messages []chatMessage, tools []toolDefinition, keepalive func()) (chatAPIResponse, error) {
	reqBody := chatAPIRequest{
		Model:     model,
		Messages:  messages,
		Tools:     tools,
		Stream:    false,
		Think:     false,
		KeepAlive: modelKeepAlive,
		Options: a.ollamaRequestOptions(model, map[string]any{
			"num_ctx":     8192,
			"num_predict": 192,
		}),
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return chatAPIResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.ollamaURL+"/api/chat", strings.NewReader(string(body)))
	if err != nil {
		return chatAPIResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := doOllamaRequest(ctx, a.client, httpReq, agentSSEKeepaliveInterval, keepalive)
	if err != nil {
		return chatAPIResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return chatAPIResponse{}, fmt.Errorf("ollama agent planner returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var out chatAPIResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&out); err != nil {
		return chatAPIResponse{}, err
	}
	if out.Error != "" {
		return chatAPIResponse{}, fmt.Errorf("ollama agent planner: %s", out.Error)
	}
	return out, nil
}

func (a *app) streamAgentFinal(ctx context.Context, w io.Writer, flusher http.Flusher, policy modelPolicy, messages []chatMessage, toolCallsUsed, plannerCalls int, agentStarted time.Time) error {
	return a.streamAgentFinalWithLimit(ctx, w, flusher, policy, messages, toolCallsUsed, plannerCalls, agentStarted, 768)
}

func (a *app) streamAgentFinalWithLimit(ctx context.Context, w io.Writer, flusher http.Flusher, policy modelPolicy, messages []chatMessage, toolCallsUsed, plannerCalls int, agentStarted time.Time, numPredict int) error {
	reqBody := chatAPIRequest{
		Model:     policy.Model,
		Messages:  messages,
		Stream:    true,
		Think:     false,
		KeepAlive: modelKeepAlive,
		Options: a.ollamaRequestOptions(policy.Model, map[string]any{
			"num_ctx":     8192,
			"num_predict": numPredict,
		}),
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.ollamaURL+"/api/chat", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := doOllamaRequest(ctx, a.client, httpReq, agentSSEKeepaliveInterval, func() {
		writeSSEKeepalive(w, flusher)
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("ollama final response returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	var final chatAPIResponse
	err = scanOllamaChat(ctx, resp.Body, agentSSEKeepaliveInterval, func() {
		writeSSEKeepalive(w, flusher)
	}, func(chunk chatAPIResponse) error {
		if chunk.Error != "" {
			return fmt.Errorf("ollama final response: %s", chunk.Error)
		}
		if chunk.Message.Content != "" {
			sendSSE(w, "token", map[string]string{"content": chunk.Message.Content})
			flusher.Flush()
		}
		if chunk.Done {
			final = chunk
		}
		return nil
	})
	if err != nil {
		return err
	}
	tokensPerSecond := 0.0
	if final.EvalDuration > 0 {
		tokensPerSecond = float64(final.EvalCount) / (float64(final.EvalDuration) / 1e9)
	}
	sendSSE(w, "done", map[string]any{
		"model":             policy.Model,
		"mode":              policy.Mode,
		"agent":             true,
		"answer_mode":       "agent",
		"model_invoked":     true,
		"tool_calls":        toolCallsUsed,
		"planner_calls":     plannerCalls,
		"agent_seconds":     round2(time.Since(agentStarted).Seconds()),
		"prompt_tokens":     final.PromptEvalCount,
		"prompt_seconds":    round2(float64(final.PromptEvalDuration) / 1e9),
		"tokens":            final.EvalCount,
		"tokens_per_second": round2(tokensPerSecond),
		"load_seconds":      round2(float64(final.LoadDuration) / 1e9),
		"total_seconds":     round2(float64(final.TotalDuration) / 1e9),
	})
	flusher.Flush()
	return nil
}

func doOllamaRequest(ctx context.Context, client *http.Client, req *http.Request, interval time.Duration, keepalive func()) (*http.Response, error) {
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := client.Do(req)
		done <- result{resp: resp, err: err}
	}()
	if keepalive == nil || interval <= 0 {
		out := <-done
		return out.resp, out.err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case out := <-done:
			return out.resp, out.err
		case <-ticker.C:
			keepalive()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func scanOllamaChat(ctx context.Context, body io.Reader, interval time.Duration, keepalive func(), handle func(chatAPIResponse) error) error {
	type result struct {
		chunk chatAPIResponse
		err   error
	}
	scanCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan result)
	go func() {
		defer close(results)
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
		for scanner.Scan() {
			var chunk chatAPIResponse
			if err := json.Unmarshal(scanner.Bytes(), &chunk); err != nil {
				continue
			}
			select {
			case results <- result{chunk: chunk}:
			case <-scanCtx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case results <- result{err: err}:
			case <-scanCtx.Done():
			}
		}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case out, ok := <-results:
			if !ok {
				return nil
			}
			if out.err != nil {
				return out.err
			}
			if err := handle(out.chunk); err != nil {
				return err
			}
		case <-ticker.C:
			if keepalive != nil {
				keepalive()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func writeSSEKeepalive(w io.Writer, flusher http.Flusher) {
	_, _ = io.WriteString(w, ": keepalive\n\n")
	flusher.Flush()
}

func emitBufferedAnswer(w io.Writer, flusher http.Flusher, content string) {
	parts := strings.Fields(content)
	for index, part := range parts {
		if index > 0 {
			part = " " + part
		}
		sendSSE(w, "token", map[string]string{"content": part})
	}
	flusher.Flush()
}
