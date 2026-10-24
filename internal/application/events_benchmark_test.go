package application

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
)

func BenchmarkDataEventReplay(b *testing.B) {
	block, err := llm.NewTextBlock(strings.Repeat("stream history ", 128))
	if err != nil {
		b.Fatal(err)
	}
	message, err := llm.NewAssistantTextMessage([]llm.TextBlock{block}, llm.FinishStop, llm.Usage{}, time.UnixMilli(1),
		llm.AssistantProvenance{Provider: "fixture", API: "openai-completions", Model: "fixture-model"})
	if err != nil {
		b.Fatal(err)
	}
	wrapped, err := agentmsg.NewLLM(message)
	if err != nil {
		b.Fatal(err)
	}
	event := Event{SessionID: "fixture", Value: AgentSessionEvent{Event: agent.MessageEndEvent{Message: wrapped}}}
	for _, replayCount := range []int{64, defaultEventHistoryCapacity} {
		b.Run(fmt.Sprintf("retained4096/replay%d", replayCount), func(b *testing.B) {
			stream := newEventStream(defaultEventHistoryCapacity)
			defer stream.close()
			for index := 0; index < defaultEventHistoryCapacity; index++ {
				stream.publish(event)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				subscription, err := stream.subscribe(uint64(defaultEventHistoryCapacity - replayCount))
				if err != nil {
					b.Fatal(err)
				}
				if len(subscription.Replay) != replayCount {
					b.Fatalf("replay count = %d", len(subscription.Replay))
				}
				subscription.Close()
			}
		})
	}
	b.Run("publishRetained4096", func(b *testing.B) {
		stream := newEventStream(defaultEventHistoryCapacity)
		defer stream.close()
		// Include rollover before timing so slice growth on the first eviction
		// does not dominate a fixed-iteration steady-state comparison.
		for index := 0; index < defaultEventHistoryCapacity+16; index++ {
			stream.publish(event)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			stream.publish(event)
		}
	})
}
