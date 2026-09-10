package provider

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/cat3399/pi-go/internal/llm"
)

// Complete collects one provider response without running tools or maintaining
// Agent state. Callers own retry and persistence policy.
func Complete(ctx context.Context, implementation Streamer, request Request) (llm.AssistantTerminal, error) {
	stream := implementation.Stream(ctx, request)
	if stream == nil || isTypedNil(stream) {
		return nil, errors.New("provider returned nil stream")
	}
	closed := false
	defer func() {
		if !closed {
			_ = stream.Close()
		}
	}()
	collector := &llm.StreamCollector{}
	for {
		event, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if err := collector.Accept(event); err != nil {
			return nil, fmt.Errorf("provider stream event: %w", err)
		}
	}
	closed = true
	if err := stream.Close(); err != nil {
		return nil, err
	}
	if err := collector.Close(); err != nil {
		return nil, err
	}
	return collector.Result()
}
