package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"

	"github.com/tiennm99/MTClaw/internal/provider"
)

// Complete sends one chat completion request built from req and translates
// the result back into our Response type. Retries on transient failures are
// the SDK's own (option.WithMaxRetries, set at construction); Complete adds
// no second retry layer.
func (c *Client) Complete(ctx context.Context, req provider.Request) (*provider.Response, error) {
	params, err := toSDK(req)
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrBadRequest, Msg: err.Error(), Err: err}
	}

	completion, err := c.sdk.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, classify(err)
	}

	return fromSDK(completion)
}

// ProbeResult reports the outcome of a minimal reachability check against
// the configured OpenAI endpoint. It never carries the API key: only the
// base URL, model, latency, and a possible error are exposed. `doctor`
// (phase 9) is the intended caller; wiring it up is deliberately out of
// scope for this phase.
type ProbeResult struct {
	BaseURL string
	Model   string
	Latency time.Duration
	Err     error
}

// Probe issues one minimal completion (max_tokens capped low) against model
// to confirm the configured endpoint and credentials are reachable.
func (c *Client) Probe(ctx context.Context, model string) ProbeResult {
	start := time.Now()
	_, err := c.sdk.Chat.Completions.New(ctx, openaisdk.ChatCompletionNewParams{
		Model:     model,
		Messages:  []openaisdk.ChatCompletionMessageParamUnion{openaisdk.UserMessage("ping")},
		MaxTokens: param.NewOpt(int64(1)),
	})

	result := ProbeResult{
		BaseURL: c.baseURL,
		Model:   model,
		Latency: time.Since(start),
	}
	if err != nil {
		result.Err = classify(err)
	}
	return result
}

// toSDK converts our Request into the SDK's ChatCompletionNewParams.
func toSDK(req provider.Request) (openaisdk.ChatCompletionNewParams, error) {
	params := openaisdk.ChatCompletionNewParams{
		Model:    req.Model,
		Messages: make([]openaisdk.ChatCompletionMessageParamUnion, 0, len(req.Messages)),
	}
	if req.Temperature != nil {
		params.Temperature = param.NewOpt(*req.Temperature)
	}

	for _, m := range req.Messages {
		sdkMsg, err := toSDKMessage(m)
		if err != nil {
			return openaisdk.ChatCompletionNewParams{}, err
		}
		params.Messages = append(params.Messages, sdkMsg)
	}

	if len(req.Tools) > 0 {
		params.Tools = make([]openaisdk.ChatCompletionToolUnionParam, 0, len(req.Tools))
		for _, t := range req.Tools {
			params.Tools = append(params.Tools, openaisdk.ChatCompletionFunctionTool(openaisdk.FunctionDefinitionParam{
				Name:        t.Name,
				Description: param.NewOpt(t.Description),
				Parameters:  openaisdk.FunctionParameters(t.Schema),
			}))
		}
	}

	return params, nil
}

// toSDKMessage converts one Message to the SDK's per-role union. The
// assistant-with-tool-calls case is the fiddly one: it must reconstruct the
// assistant param (including each tool call's id/name/arguments) rather than
// relying on the SDK's own ChatCompletionMessage.ToParam, since our stored
// representation is provider.Message, not the SDK's response type.
func toSDKMessage(m provider.Message) (openaisdk.ChatCompletionMessageParamUnion, error) {
	switch m.Role {
	case provider.RoleSystem:
		return openaisdk.SystemMessage(m.Content), nil

	case provider.RoleUser:
		return openaisdk.UserMessage(m.Content), nil

	case provider.RoleAssistant:
		if len(m.ToolCalls) == 0 {
			return openaisdk.AssistantMessage(m.Content), nil
		}

		var assistant openaisdk.ChatCompletionAssistantMessageParam
		if m.Content != "" {
			assistant.Content.OfString = param.NewOpt(m.Content)
		}
		assistant.ToolCalls = make([]openaisdk.ChatCompletionMessageToolCallUnionParam, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			assistant.ToolCalls = append(assistant.ToolCalls, openaisdk.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openaisdk.ChatCompletionMessageFunctionToolCallParam{
					ID: tc.ID,
					Function: openaisdk.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name:      tc.Name,
						Arguments: string(tc.Args),
					},
				},
			})
		}
		return openaisdk.ChatCompletionMessageParamUnion{OfAssistant: &assistant}, nil

	case provider.RoleTool:
		return openaisdk.ToolMessage(m.Content, m.ToolCallID), nil

	default:
		return openaisdk.ChatCompletionMessageParamUnion{}, fmt.Errorf("openai: unknown message role %q", m.Role)
	}
}

// fromSDK converts the SDK's completion response into our Response. Empty
// Choices is treated as a bad-request-shaped error rather than an index
// panic - it should never happen for n=1, but a defensive check is cheap.
func fromSDK(completion *openaisdk.ChatCompletion) (*provider.Response, error) {
	if completion == nil || len(completion.Choices) == 0 {
		return nil, &provider.Error{Kind: provider.ErrBadRequest, Msg: "openai: response contained no choices"}
	}

	choice := completion.Choices[0]
	msg := provider.Message{
		Role:    provider.RoleAssistant,
		Content: choice.Message.Content,
	}
	if len(choice.Message.ToolCalls) > 0 {
		msg.ToolCalls = make([]provider.ToolCall, 0, len(choice.Message.ToolCalls))
		for _, tc := range choice.Message.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, provider.ToolCall{
				ID:   tc.ID,
				Name: tc.Function.Name,
				Args: json.RawMessage(tc.Function.Arguments),
			})
		}
	}

	return &provider.Response{
		Message: msg,
		Usage: provider.Usage{
			Prompt:     int(completion.Usage.PromptTokens),
			Completion: int(completion.Usage.CompletionTokens),
		},
		FinishReason: choice.FinishReason,
	}, nil
}
