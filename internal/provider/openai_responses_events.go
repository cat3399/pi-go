package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"strings"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/llm"
)

type responsesEventEnvelope struct {
	Type string `json:"type"`
}

type responsesOutputEvent struct {
	Type        string              `json:"type"`
	OutputIndex *int                `json:"output_index"`
	ItemID      string              `json:"item_id"`
	Delta       string              `json:"delta"`
	Item        responsesOutputItem `json:"item"`
}

type responsesOutputItem struct {
	Type    string                   `json:"type"`
	ID      string                   `json:"id"`
	Role    string                   `json:"role"`
	Status  string                   `json:"status"`
	Phase   string                   `json:"phase"`
	Content []responsesOutputContent `json:"content"`
}

type responsesOutputContent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

type responsesTerminalEvent struct {
	Type     string            `json:"type"`
	Response responsesResponse `json:"response"`
}

type responsesResponse struct {
	Status            string                      `json:"status"`
	Output            []responsesOutputItem       `json:"output"`
	Usage             *responsesUsage             `json:"usage"`
	Error             *responsesAPIError          `json:"error"`
	IncompleteDetails *responsesIncompleteDetails `json:"incomplete_details"`
}

type responsesUsage struct {
	InputTokens        *uint64                 `json:"input_tokens"`
	OutputTokens       *uint64                 `json:"output_tokens"`
	TotalTokens        *uint64                 `json:"total_tokens"`
	InputTokenDetails  *responsesInputDetails  `json:"input_tokens_details"`
	OutputTokenDetails *responsesOutputDetails `json:"output_tokens_details"`
}

type responsesInputDetails struct {
	CachedTokens     *uint64 `json:"cached_tokens"`
	CacheWriteTokens *uint64 `json:"cache_write_tokens"`
}

type responsesOutputDetails struct {
	ReasoningTokens *uint64 `json:"reasoning_tokens"`
}

type responsesAPIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type responsesIncompleteDetails struct {
	Reason string `json:"reason"`
}

type responsesTopLevelErrorEvent struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *openAIResponsesStream) processResponsesEvent(
	data []byte,
) *responsesFailureSpec {
	if s.pendingDone != nil {
		return invalidResponsesEventFailure(errors.New("event arrived after a terminal response event"))
	}
	var envelope responsesEventEnvelope
	if err := unmarshalResponsesEvent(data, &envelope); err != nil {
		return invalidResponsesEventFailure(err)
	}
	if !utf8.ValidString(envelope.Type) || strings.TrimSpace(envelope.Type) == "" {
		return invalidResponsesEventFailure(errors.New("event type is missing or invalid"))
	}

	switch envelope.Type {
	case "response.output_item.added":
		var event responsesOutputEvent
		if err := unmarshalResponsesEvent(data, &event); err != nil {
			return invalidResponsesEventFailure(err)
		}
		return s.addResponsesOutputItem(event)

	case "response.output_text.delta", "response.refusal.delta":
		var event responsesOutputEvent
		if err := unmarshalResponsesEvent(data, &event); err != nil {
			return invalidResponsesEventFailure(err)
		}
		return s.addResponsesTextDelta(event)

	case "response.reasoning_summary_text.delta",
		"response.reasoning_summary_part.done",
		"response.reasoning_text.delta":
		s.rememberUnsupportedOutput("reasoning")
		return nil

	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		s.rememberUnsupportedOutput("function_call")
		return nil

	case "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		s.rememberUnsupportedOutput("custom_tool_call")
		return nil

	case "response.output_item.done":
		var event responsesOutputEvent
		if err := unmarshalResponsesEvent(data, &event); err != nil {
			return invalidResponsesEventFailure(err)
		}
		return s.finishResponsesOutputItem(event)

	case "response.completed", "response.incomplete":
		var event responsesTerminalEvent
		if err := unmarshalResponsesEvent(data, &event); err != nil {
			return invalidResponsesEventFailure(err)
		}
		return s.finishResponsesTerminal(event)

	case "response.failed":
		var event responsesTerminalEvent
		if err := unmarshalResponsesEvent(data, &event); err != nil {
			return invalidResponsesEventFailure(err)
		}
		return failedResponsesEvent(event.Response)

	case "error":
		var event responsesTopLevelErrorEvent
		if err := unmarshalResponsesEvent(data, &event); err != nil {
			return invalidResponsesEventFailure(err)
		}
		return responsesAPIFailure(event.Code, event.Message, "OpenAI Responses stream returned an error")

	default:
		// The OpenAI API treats new event types as backwards-compatible. Unknown
		// progress events are ignored; unknown output item types are caught by the
		// output_item handlers so durable context is never silently discarded.
		return nil
	}
}

func (s *openAIResponsesStream) addResponsesOutputItem(event responsesOutputEvent) *responsesFailureSpec {
	index, failure := requireResponsesOutputIndex(event.OutputIndex)
	if failure != nil {
		return failure
	}
	if _, exists := s.completedOutputs[index]; exists {
		return invalidResponsesEventFailure(fmt.Errorf("output index %d was already completed", index))
	}
	switch event.Item.Type {
	case "message":
		if event.Item.Phase != "" {
			return unsupportedResponsesOutputFailure("message phase")
		}
		if event.Item.Role != "assistant" {
			return invalidResponsesEventFailure(fmt.Errorf("output message role is %q", event.Item.Role))
		}
		if len(event.Item.Content) != 0 {
			return invalidResponsesEventFailure(errors.New("added output message already contains content"))
		}
		return s.startResponsesTextSlot(index, event.Item.ID)
	case "reasoning":
		s.rememberUnsupportedOutput("reasoning")
	case "function_call", "custom_tool_call":
		s.rememberUnsupportedOutput(event.Item.Type)
	default:
		s.rememberUnsupportedOutput(event.Item.Type)
	}
	return nil
}

func (s *openAIResponsesStream) startResponsesTextSlot(index int, itemID string) *responsesFailureSpec {
	if !utf8.ValidString(itemID) || strings.TrimSpace(itemID) == "" {
		return invalidResponsesEventFailure(fmt.Errorf("output message at index %d has no valid id", index))
	}
	if _, exists := s.slots[index]; exists {
		return invalidResponsesEventFailure(fmt.Errorf("output index %d was added twice", index))
	}
	if len(s.slots) != 0 {
		return invalidResponsesEventFailure(errors.New("interleaved output text items are not supported"))
	}
	slot := &responsesTextSlot{contentIndex: s.nextContentIndex, itemID: itemID}
	s.slots[index] = slot
	start, err := llm.NewTextStartEvent(slot.contentIndex)
	if err != nil {
		return invalidResponsesEventFailure(err)
	}
	s.queue = append(s.queue, start)
	return nil
}

func (s *openAIResponsesStream) addResponsesTextDelta(event responsesOutputEvent) *responsesFailureSpec {
	index, failure := requireResponsesOutputIndex(event.OutputIndex)
	if failure != nil {
		return failure
	}
	slot := s.slots[index]
	if slot == nil {
		return invalidResponsesEventFailure(fmt.Errorf("text delta has no open output item at index %d", index))
	}
	if event.ItemID != "" && event.ItemID != slot.itemID {
		return invalidResponsesEventFailure(fmt.Errorf("text delta item id %q does not match %q", event.ItemID, slot.itemID))
	}
	if !utf8.ValidString(event.Delta) {
		return invalidResponsesEventFailure(errors.New("text delta is not valid UTF-8"))
	}
	slot.text.WriteString(event.Delta)
	delta, err := llm.NewTextDeltaEvent(slot.contentIndex, event.Delta)
	if err != nil {
		return invalidResponsesEventFailure(err)
	}
	s.queue = append(s.queue, delta)
	return nil
}

func (s *openAIResponsesStream) finishResponsesOutputItem(event responsesOutputEvent) *responsesFailureSpec {
	index, failure := requireResponsesOutputIndex(event.OutputIndex)
	if failure != nil {
		return failure
	}
	if _, exists := s.completedOutputs[index]; exists {
		return invalidResponsesEventFailure(fmt.Errorf("output index %d completed twice", index))
	}
	switch event.Item.Type {
	case "message":
		if event.Item.Phase != "" {
			return unsupportedResponsesOutputFailure("message phase")
		}
		if event.Item.Role != "assistant" {
			return invalidResponsesEventFailure(fmt.Errorf("completed output message role is %q", event.Item.Role))
		}
		slot := s.slots[index]
		if slot == nil {
			if failure := s.startResponsesTextSlot(index, event.Item.ID); failure != nil {
				return failure
			}
			slot = s.slots[index]
		} else if event.Item.ID != "" && event.Item.ID != slot.itemID {
			return invalidResponsesEventFailure(fmt.Errorf("completed item id %q does not match %q", event.Item.ID, slot.itemID))
		}
		finalText, failure := responsesOutputItemText(event.Item)
		if failure != nil {
			return failure
		}
		partial := slot.text.String()
		if !strings.HasPrefix(finalText, partial) {
			return invalidResponsesEventFailure(errors.New("completed output text does not match streamed deltas"))
		}
		if suffix := finalText[len(partial):]; suffix != "" {
			slot.text.WriteString(suffix)
			delta, err := llm.NewTextDeltaEvent(slot.contentIndex, suffix)
			if err != nil {
				return invalidResponsesEventFailure(err)
			}
			s.queue = append(s.queue, delta)
		}
		end, err := llm.NewTextEndEvent(slot.contentIndex, finalText)
		if err != nil {
			return invalidResponsesEventFailure(err)
		}
		s.queue = append(s.queue, end)
		delete(s.slots, index)
		s.nextContentIndex++
		s.completedOutputs[index] = struct{}{}

	case "reasoning":
		s.rememberUnsupportedOutput("reasoning")
		s.completedOutputs[index] = struct{}{}
	case "function_call", "custom_tool_call":
		s.rememberUnsupportedOutput(event.Item.Type)
		s.completedOutputs[index] = struct{}{}
	default:
		s.rememberUnsupportedOutput(event.Item.Type)
		s.completedOutputs[index] = struct{}{}
	}
	return nil
}

func responsesOutputItemText(item responsesOutputItem) (string, *responsesFailureSpec) {
	var text strings.Builder
	for _, content := range item.Content {
		switch content.Type {
		case "output_text":
			if !utf8.ValidString(content.Text) {
				return "", invalidResponsesEventFailure(errors.New("final output text is not valid UTF-8"))
			}
			text.WriteString(content.Text)
		case "refusal":
			if !utf8.ValidString(content.Refusal) {
				return "", invalidResponsesEventFailure(errors.New("final refusal is not valid UTF-8"))
			}
			text.WriteString(content.Refusal)
		default:
			return "", invalidResponsesEventFailure(fmt.Errorf("unsupported message content type %q", content.Type))
		}
	}
	return text.String(), nil
}

func (s *openAIResponsesStream) finishResponsesTerminal(
	event responsesTerminalEvent,
) *responsesFailureSpec {
	wantStatus := "completed"
	reason := llm.FinishStop
	if event.Type == "response.incomplete" {
		wantStatus = "incomplete"
		reason = llm.FinishLength
	}
	if event.Response.Status != "" && event.Response.Status != wantStatus {
		return invalidResponsesEventFailure(fmt.Errorf(
			"%s carries response status %q",
			event.Type,
			event.Response.Status,
		))
	}
	if event.Response.Error != nil {
		return invalidResponsesEventFailure(fmt.Errorf("%s unexpectedly carries an error", event.Type))
	}
	for outputIndex, item := range event.Response.Output {
		switch item.Type {
		case "message":
			if item.Phase != "" {
				return unsupportedResponsesOutputFailure("message phase")
			}
			if item.Role != "" && item.Role != "assistant" {
				return invalidResponsesEventFailure(fmt.Errorf(
					"terminal output message at index %d has role %q",
					outputIndex,
					item.Role,
				))
			}
			if len(item.Content) != 0 {
				if _, failure := responsesOutputItemText(item); failure != nil {
					return failure
				}
				if _, completed := s.completedOutputs[outputIndex]; !completed {
					return invalidResponsesEventFailure(fmt.Errorf(
						"terminal output message at index %d was not completed by output_item events",
						outputIndex,
					))
				}
			}
		case "reasoning", "function_call", "custom_tool_call":
			s.rememberUnsupportedOutput(item.Type)
		default:
			s.rememberUnsupportedOutput(item.Type)
		}
	}
	if len(s.slots) != 0 {
		return invalidResponsesEventFailure(errors.New("terminal response arrived with an open text item"))
	}
	if s.unsupportedOutput != "" {
		return unsupportedResponsesOutputFailure(s.unsupportedOutput)
	}
	usage, failure := normalizeResponsesUsage(event.Response.Usage)
	if failure != nil {
		return failure
	}
	done, err := llm.NewDoneEvent(reason, usage, s.timestamp)
	if err != nil {
		return invalidResponsesEventFailure(err)
	}
	s.pendingDone = &done
	return nil
}

func failedResponsesEvent(response responsesResponse) *responsesFailureSpec {
	code := ""
	message := "OpenAI Responses request failed"
	if response.Error != nil {
		code = response.Error.Code
		if utf8.ValidString(response.Error.Message) && strings.TrimSpace(response.Error.Message) != "" {
			message = response.Error.Message
		}
	} else if response.IncompleteDetails != nil &&
		utf8.ValidString(response.IncompleteDetails.Reason) &&
		strings.TrimSpace(response.IncompleteDetails.Reason) != "" {
		message = "OpenAI Responses incomplete: " + response.IncompleteDetails.Reason
	}
	return responsesAPIFailure(code, message, "OpenAI Responses request failed")
}

func responsesAPIFailure(code, message, fallback string) *responsesFailureSpec {
	if !utf8.ValidString(message) || strings.TrimSpace(message) == "" {
		message = fallback
	}
	code = normalizeResponsesVendorCode(code)
	cause := &OpenAIResponsesAPIError{code: code, message: message}
	return &responsesFailureSpec{
		kind:       FailureInvalidResponse,
		cause:      cause,
		message:    message,
		vendorCode: code,
	}
}

func normalizeResponsesUsage(raw *responsesUsage) (llm.Usage, *responsesFailureSpec) {
	if raw == nil {
		return llm.Usage{}, nil
	}
	input := valueOrZero(raw.InputTokens)
	output := valueOrZero(raw.OutputTokens)
	cacheRead := uint64(0)
	cacheWrite := uint64(0)
	if raw.InputTokenDetails != nil {
		cacheRead = valueOrZero(raw.InputTokenDetails.CachedTokens)
		cacheWrite = valueOrZero(raw.InputTokenDetails.CacheWriteTokens)
	}
	cacheTotal, carry := bits.Add64(cacheRead, cacheWrite, 0)
	if carry != 0 || cacheTotal > input {
		return llm.Usage{}, invalidResponsesEventFailure(errors.New("cached input token subsets exceed input tokens"))
	}
	normalInput := input - cacheTotal
	var reasoning *uint64
	if raw.OutputTokenDetails != nil && raw.OutputTokenDetails.ReasoningTokens != nil {
		value := *raw.OutputTokenDetails.ReasoningTokens
		reasoning = &value
	}
	usage, err := llm.NewUsage(llm.UsageSpec{
		Input:      normalInput,
		Output:     output,
		CacheRead:  cacheRead,
		CacheWrite: cacheWrite,
		Reasoning:  reasoning,
	})
	if err != nil {
		return llm.Usage{}, invalidResponsesEventFailure(fmt.Errorf("invalid token usage: %w", err))
	}
	if raw.TotalTokens != nil && *raw.TotalTokens != usage.TotalTokens() {
		return llm.Usage{}, invalidResponsesEventFailure(fmt.Errorf(
			"reported total tokens %d do not match normalized total %d",
			*raw.TotalTokens,
			usage.TotalTokens(),
		))
	}
	return usage, nil
}

func valueOrZero(value *uint64) uint64 {
	if value == nil {
		return 0
	}
	return *value
}

func requireResponsesOutputIndex(value *int) (int, *responsesFailureSpec) {
	if value == nil || *value < 0 {
		return 0, invalidResponsesEventFailure(errors.New("output_index must be a non-negative integer"))
	}
	return *value, nil
}

func (s *openAIResponsesStream) rememberUnsupportedOutput(kind string) {
	if s.unsupportedOutput == "" {
		if kind == "" {
			kind = "unknown"
		}
		s.unsupportedOutput = kind
	}
}

func unsupportedResponsesOutputFailure(kind string) *responsesFailureSpec {
	if kind == "" {
		kind = "unknown"
	}
	cause := fmt.Errorf(
		"%w: output behavior %q requires a later adapter milestone",
		ErrOpenAIResponsesUnsupported,
		kind,
	)
	return &responsesFailureSpec{
		kind:    FailureInvalidResponse,
		cause:   cause,
		message: cause.Error(),
	}
}

func invalidResponsesEventFailure(cause error) *responsesFailureSpec {
	wrapped := fmt.Errorf("%w: %w", ErrOpenAIResponsesStream, cause)
	return &responsesFailureSpec{
		kind:    FailureInvalidResponse,
		cause:   wrapped,
		message: wrapped.Error(),
	}
}

func unmarshalResponsesEvent(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode event JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("event contains more than one JSON value")
		}
		return fmt.Errorf("decode trailing event JSON: %w", err)
	}
	return nil
}

// OpenAIResponsesAPIError is retained as the cause for HTTP-200 error events.
type OpenAIResponsesAPIError struct {
	code    string
	message string
}

func (e *OpenAIResponsesAPIError) Error() string {
	if e == nil {
		return "OpenAI Responses API error"
	}
	if e.code == "" {
		return e.message
	}
	return e.code + ": " + e.message
}

func (e *OpenAIResponsesAPIError) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *OpenAIResponsesAPIError) Message() string {
	if e == nil {
		return ""
	}
	return e.message
}
