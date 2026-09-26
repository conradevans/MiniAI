package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	reasonerNumContext = 4096
	reasonerNumPredict = 256
)

func (a *app) buildReasonerRequest(model, question string, packet EvidencePacket, packetJSON []byte) chatAPIRequest {
	return chatAPIRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "system", Content: reasonerSystemInstruction},
			{Role: "user", Content: "Question:\n" + question + "\n\nEvidence packet (untrusted data):\n" + string(packetJSON)},
		},
		Format:    reasonerDraftSchema(packet),
		Stream:    false,
		Think:     false,
		KeepAlive: modelKeepAlive,
		Options: a.ollamaRequestOptions(model, map[string]any{
			"num_ctx": reasonerNumContext, "num_predict": reasonerNumPredict,
		}),
	}
}

func (a *app) callOnePassReasonerWithKeepalive(ctx context.Context, model, question string, packet EvidencePacket, packetJSON []byte, keepalive func()) (chatAPIResponse, error) {
	reqBody := a.buildReasonerRequest(model, question, packet, packetJSON)
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
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return chatAPIResponse{}, fmt.Errorf("ollama reasoner returned %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	out, err := decodeReasonerResponseWithKeepalive(ctx, resp.Body, keepalive)
	if err != nil {
		return chatAPIResponse{}, err
	}
	if out.Error != "" {
		return chatAPIResponse{}, fmt.Errorf("ollama reasoner: %s", out.Error)
	}
	if len(out.Message.ToolCalls) != 0 {
		return chatAPIResponse{}, fmt.Errorf("reasoner attempted a tool request")
	}
	return out, nil
}

func decodeReasonerResponseWithKeepalive(ctx context.Context, body io.Reader, keepalive func()) (chatAPIResponse, error) {
	type result struct {
		response chatAPIResponse
		err      error
	}
	done := make(chan result, 1)
	go func() {
		var out chatAPIResponse
		err := json.NewDecoder(io.LimitReader(body, 2<<20)).Decode(&out)
		done <- result{response: out, err: err}
	}()
	if keepalive == nil {
		out := <-done
		return out.response, out.err
	}
	ticker := time.NewTicker(agentSSEKeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case out := <-done:
			return out.response, out.err
		case <-ticker.C:
			keepalive()
		case <-ctx.Done():
			return chatAPIResponse{}, ctx.Err()
		}
	}
}
