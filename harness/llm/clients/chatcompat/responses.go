package chatcompat

import (
	"context"
	"fmt"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

const defaultResponsesUserAgent = "opencode/1.18.33 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14"

// Muse uses Zen's Responses endpoint, not its Chat Completions endpoint.
// Construct the adapter per turn so x-opencode-request is fresh each time.
func (client *Client) respondResponses(ctx context.Context, request llm.Request) (llm.Response, error) {
	requestID := client.requestID
	if requestID == "" {
		requestID = opencodeID("msg_", false)
	}
	userAgent := client.userAgent
	if userAgent == DefaultUserAgent {
		userAgent = defaultResponsesUserAgent
	}
	if request.Model.MaxOutputTokens == nil {
		maxOutputTokens := int64(32000)
		request.Model.MaxOutputTokens = &maxOutputTokens
	}
	remote := primitives.NewRemoteClient()
	defer func() { _ = remote.Close() }()
	adapter, err := responsesapi.NewAdapter(remote, responsesapi.Config{
		Endpoint: client.baseURL + "/responses",
		Headers: map[string][]string{
			"Authorization":      {"Bearer " + client.apiKey},
			"Content-Type":       {"application/json"},
			"User-Agent":         {userAgent},
			"x-opencode-client":  {client.clientType},
			"x-opencode-session": {client.sessionID},
			"x-opencode-request": {requestID},
			"x-opencode-project": {client.projectID},
		},
		MaxAttempts:       &client.maxAttempts,
		CacheKeyPlacement: responsesapi.CacheKeyPlacement{UsePromptCacheKeyField: true},
	})
	if err != nil {
		return llm.Response{}, fmt.Errorf("configure Zen Responses API: %w", err)
	}
	return adapter.Respond(ctx, request, llm.RequestOptions{})
}
