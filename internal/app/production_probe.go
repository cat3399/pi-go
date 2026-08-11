package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	modelcatalog "github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/provider"
)

type ProductionModelProbeResult struct {
	Latency      time.Duration
	Status       int
	ResponseText string
}

// ProbeProductionModel runs an unsaved models.json draft through the same auth
// resolver and API adapter graph as a production Agent session. It deliberately
// sends no tools and persists no Session state.
func ProbeProductionModel(
	ctx context.Context,
	config ProductionConfig,
	configured modelcatalog.ProviderConfig,
	selected provider.Model,
) (ProductionModelProbeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	paths, err := ResolveProductionPaths(config)
	if err != nil {
		return ProductionModelProbeResult{}, err
	}
	environment := config.Environment
	if environment == nil {
		environment = os.Environ()
	}
	resolver, err := newProductionProviderAuthResolver(paths.AgentDir, environmentMap(environment), config)
	if err != nil {
		return ProductionModelProbeResult{}, err
	}
	resolved, err := resolver.Resolve(ctx, configured, selected, modelcatalog.AuthOverrides{})
	if err != nil {
		return ProductionModelProbeResult{}, err
	}
	if resolved == nil {
		return ProductionModelProbeResult{}, fmt.Errorf("no API key found for %q", configured.ID)
	}
	adapters, err := newProductionProviderAdapters(config)
	if err != nil {
		return ProductionModelProbeResult{}, err
	}
	adapter := adapters[selected.API()]
	if adapter == nil {
		return ProductionModelProbeResult{}, fmt.Errorf("unsupported model API: %s", selected.API())
	}
	if validator, ok := adapter.(provider.RouteValidator); ok && !validator.SupportsModel(selected) {
		return ProductionModelProbeResult{}, fmt.Errorf("unsupported model route: %s/%s", selected.Provider(), selected.ID())
	}
	message, err := llm.NewUserTextMessage("Reply with OK only.", time.Now())
	if err != nil {
		return ProductionModelProbeResult{}, err
	}
	maxTokens, timeoutMS, maxRetries := uint64(16), uint64(20_000), uint32(0)
	status := 0
	request, err := provider.NewRequestWithOptions(selected, "", []llm.ConversationMessage{message}, provider.RequestOptions{
		ThinkingLevel: provider.ThinkingOff,
		Stream: provider.StreamOptions{
			APIKey: resolved.APIKey, Headers: resolved.Headers, Env: resolved.Env,
			MaxTokens: &maxTokens, TimeoutMS: &timeoutMS, MaxRetries: &maxRetries,
			CacheRetention: provider.CacheRetentionNone,
			OnResponse: func(_ provider.Model, response provider.ResponseInfo) error {
				status = response.StatusCode
				return nil
			},
		},
	})
	if err != nil {
		return ProductionModelProbeResult{}, err
	}
	started := time.Now()
	stream := adapter.Stream(ctx, request)
	if stream == nil {
		return ProductionModelProbeResult{}, errors.New("model adapter returned no stream")
	}
	collector := &llm.StreamCollector{}
	for {
		event, nextErr := stream.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			_ = stream.Close()
			return ProductionModelProbeResult{Latency: time.Since(started), Status: status}, nextErr
		}
		if err := collector.Accept(event); err != nil {
			_ = stream.Close()
			return ProductionModelProbeResult{Latency: time.Since(started), Status: status}, err
		}
	}
	if err := stream.Close(); err != nil {
		return ProductionModelProbeResult{Latency: time.Since(started), Status: status}, err
	}
	if err := collector.Close(); err != nil {
		return ProductionModelProbeResult{Latency: time.Since(started), Status: status}, err
	}
	terminal, err := collector.Result()
	if err != nil {
		return ProductionModelProbeResult{Latency: time.Since(started), Status: status}, err
	}
	if failure, ok := terminal.(llm.AssistantFailureMessage); ok {
		return ProductionModelProbeResult{Latency: time.Since(started), Status: status}, errors.New(failure.ErrorMessage())
	}
	var text strings.Builder
	for _, block := range terminal.Blocks() {
		if content, ok := block.(llm.TextBlock); ok {
			text.WriteString(content.Text())
		}
	}
	responseText := []rune(text.String())
	if len(responseText) > 300 {
		responseText = responseText[:300]
	}
	return ProductionModelProbeResult{
		Latency: time.Since(started), Status: status, ResponseText: string(responseText),
	}, nil
}
