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
	Text        string              `json:"text"`
	CallID      string              `json:"call_id"`
	Name        string              `json:"name"`
	Arguments   string              `json:"arguments"`
	Item        responsesOutputItem `json:"item"`
}

type responsesOutputItem struct {
	Type             string                   `json:"type"`
	ID               string                   `json:"id"`
	Role             string                   `json:"role"`
	Status           string                   `json:"status"`
	Phase            string                   `json:"phase"`
	CallID           string                   `json:"call_id"`
	Name             string                   `json:"name"`
	Arguments        string                   `json:"arguments"`
	Content          []responsesOutputContent `json:"content"`
	PlaintextContent string                   `json:"-"`
	Summary          []responsesOutputContent `json:"summary"`
	EncryptedContent string                   `json:"encrypted_content"`
}

// Some Responses-compatible endpoints represent reasoning content as a
// string, while OpenAI uses an array for message/reasoning content. Preserve
// this narrow wire variance without making arbitrary item JSON opaque.
func (item *responsesOutputItem) UnmarshalJSON(data []byte) error {
	type itemAlias responsesOutputItem
	var wire struct {
		Type             string                   `json:"type"`
		ID               string                   `json:"id"`
		Role             string                   `json:"role"`
		Status           string                   `json:"status"`
		Phase            string                   `json:"phase"`
		CallID           string                   `json:"call_id"`
		Name             string                   `json:"name"`
		Arguments        string                   `json:"arguments"`
		Content          json.RawMessage          `json:"content"`
		Summary          []responsesOutputContent `json:"summary"`
		EncryptedContent string                   `json:"encrypted_content"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*item = responsesOutputItem{Type: wire.Type, ID: wire.ID, Role: wire.Role, Status: wire.Status, Phase: wire.Phase, CallID: wire.CallID, Name: wire.Name, Arguments: wire.Arguments, Summary: wire.Summary, EncryptedContent: wire.EncryptedContent}
	if len(wire.Content) == 0 || bytes.Equal(bytes.TrimSpace(wire.Content), []byte("null")) {
		return nil
	}
	if err := json.Unmarshal(wire.Content, &item.Content); err == nil {
		return nil
	}
	if err := json.Unmarshal(wire.Content, &item.PlaintextContent); err != nil {
		return fmt.Errorf("content must be an array or string: %w", err)
	}
	return nil
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
	ID                string                      `json:"id"`
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

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		var event responsesOutputEvent
		if err := unmarshalResponsesEvent(data, &event); err != nil {
			return invalidResponsesEventFailure(err)
		}
		return s.addResponsesReasoningDelta(event)
	case "response.output_text.done", "response.refusal.done":
		var event responsesOutputEvent
		if err := unmarshalResponsesEvent(data, &event); err != nil {
			return invalidResponsesEventFailure(err)
		}
		return s.finishResponsesTextProgress(event)
	case "response.reasoning_summary_text.done", "response.reasoning_text.done":
		var event responsesOutputEvent
		if err := unmarshalResponsesEvent(data, &event); err != nil {
			return invalidResponsesEventFailure(err)
		}
		return s.finishResponsesReasoningProgress(event)
	case "response.reasoning_summary_part.done":
		var event responsesOutputEvent
		if err := unmarshalResponsesEvent(data, &event); err != nil {
			return invalidResponsesEventFailure(err)
		}
		return s.addResponsesReasoningSeparator(event)

	case "response.function_call_arguments.delta":
		var event responsesOutputEvent
		if err := unmarshalResponsesEvent(data, &event); err != nil {
			return invalidResponsesEventFailure(err)
		}
		return s.addResponsesToolDelta(event)

	case "response.function_call_arguments.done":
		var event responsesOutputEvent
		if err := unmarshalResponsesEvent(data, &event); err != nil {
			return invalidResponsesEventFailure(err)
		}
		return s.finishResponsesToolArguments(event)

	case "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		return unsupportedResponsesOutputFailure("custom_tool_call")

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

	case "response.created", "response.in_progress", "response.content_part.added", "response.content_part.done":
		// Known progress events in the supported subset carry no durable block.
		return nil

	default:
		return invalidResponsesEventFailure(fmt.Errorf("unknown SSE event type %q", envelope.Type))
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
	if index != s.nextOutputIndex {
		return invalidResponsesEventFailure(fmt.Errorf("output item index %d, want %d", index, s.nextOutputIndex))
	}
	if s.sawFinalAnswer {
		return invalidResponsesEventFailure(errors.New("output item arrived after a final_answer message"))
	}
	if len(s.slots) != 0 || len(s.toolSlots) != 0 || len(s.reasoningSlots) != 0 {
		return invalidResponsesEventFailure(errors.New("output item started before previous item completed"))
	}
	switch event.Item.Type {
	case "message":
		if event.Item.Role != "assistant" {
			return invalidResponsesEventFailure(fmt.Errorf("output message role is %q", event.Item.Role))
		}
		if len(event.Item.Content) != 0 {
			return invalidResponsesEventFailure(errors.New("added output message already contains content"))
		}
		return s.startResponsesTextSlot(index, event.Item.ID, event.Item.Phase)
	case "function_call":
		return s.startResponsesToolSlot(index, event.Item.ID, event.Item.CallID, event.Item.Name, event.Item.Arguments)
	case "reasoning":
		return s.startResponsesReasoningSlot(index, event.Item.ID)
	case "custom_tool_call":
		return unsupportedResponsesOutputFailure(event.Item.Type)
	default:
		return unsupportedResponsesOutputFailure(event.Item.Type)
	}
}

func (s *openAIResponsesStream) startResponsesReasoningSlot(index int, itemID string) *responsesFailureSpec {
	if !utf8.ValidString(itemID) || strings.TrimSpace(itemID) == "" {
		return invalidResponsesEventFailure(errors.New("reasoning item has no id"))
	}
	slot := &responsesReasoningSlot{contentIndex: s.nextContentIndex, itemID: itemID}
	s.reasoningSlots[index] = slot
	start, err := llm.NewThinkingStartEvent(slot.contentIndex)
	if err != nil {
		return invalidResponsesEventFailure(err)
	}
	s.enqueueResponsesEvent(start)
	return nil
}
func (s *openAIResponsesStream) addResponsesReasoningDelta(event responsesOutputEvent) *responsesFailureSpec {
	index, f := requireResponsesOutputIndex(event.OutputIndex)
	if f != nil {
		return f
	}
	slot := s.reasoningSlots[index]
	if slot == nil {
		return invalidResponsesEventFailure(errors.New("reasoning delta has no open item"))
	}
	if event.ItemID != "" && event.ItemID != slot.itemID {
		return invalidResponsesEventFailure(errors.New("reasoning delta item id mismatch"))
	}
	if !utf8.ValidString(event.Delta) {
		return invalidResponsesEventFailure(errors.New("reasoning delta invalid UTF-8"))
	}
	slot.text.WriteString(event.Delta)
	delta, err := llm.NewThinkingDeltaEvent(slot.contentIndex, event.Delta)
	if err != nil {
		return invalidResponsesEventFailure(err)
	}
	s.enqueueResponsesEvent(delta)
	return nil
}

func responsesDoneText(event responsesOutputEvent) string {
	if event.Text != "" {
		return event.Text
	}
	return event.Delta
}

// The `*.done` events complete a streamed content part, not an output item.
// They must agree with the open slot but never emit a second end event; the
// authoritative durable close remains response.output_item.done.
func (s *openAIResponsesStream) finishResponsesTextProgress(event responsesOutputEvent) *responsesFailureSpec {
	index, failure := requireResponsesOutputIndex(event.OutputIndex)
	if failure != nil {
		return failure
	}
	slot := s.slots[index]
	if slot == nil {
		return invalidResponsesEventFailure(fmt.Errorf("text done has no open output item at index %d", index))
	}
	if event.ItemID != "" && event.ItemID != slot.itemID {
		return invalidResponsesEventFailure(fmt.Errorf("text done item id %q does not match %q", event.ItemID, slot.itemID))
	}
	text := responsesDoneText(event)
	if !utf8.ValidString(text) {
		return invalidResponsesEventFailure(errors.New("text done is not valid UTF-8"))
	}
	if text != "" && !strings.HasPrefix(text, slot.text.String()) && !strings.HasPrefix(slot.text.String(), text) {
		return invalidResponsesEventFailure(errors.New("completed text does not match streamed deltas"))
	}
	return nil
}

func (s *openAIResponsesStream) finishResponsesReasoningProgress(event responsesOutputEvent) *responsesFailureSpec {
	index, failure := requireResponsesOutputIndex(event.OutputIndex)
	if failure != nil {
		return failure
	}
	slot := s.reasoningSlots[index]
	if slot == nil {
		return invalidResponsesEventFailure(fmt.Errorf("reasoning done has no open item at index %d", index))
	}
	if event.ItemID != "" && event.ItemID != slot.itemID {
		return invalidResponsesEventFailure(errors.New("reasoning done item id mismatch"))
	}
	text := responsesDoneText(event)
	if !utf8.ValidString(text) {
		return invalidResponsesEventFailure(errors.New("reasoning done is not valid UTF-8"))
	}
	if text != "" && !strings.HasPrefix(text, slot.text.String()) && !strings.HasPrefix(slot.text.String(), text) {
		return invalidResponsesEventFailure(errors.New("completed reasoning does not match streamed deltas"))
	}
	return nil
}
func (s *openAIResponsesStream) addResponsesReasoningSeparator(event responsesOutputEvent) *responsesFailureSpec {
	index, f := requireResponsesOutputIndex(event.OutputIndex)
	if f != nil {
		return f
	}
	slot := s.reasoningSlots[index]
	if slot == nil {
		return invalidResponsesEventFailure(errors.New("reasoning separator has no open item"))
	}
	slot.text.WriteString("\n\n")
	delta, err := llm.NewThinkingDeltaEvent(slot.contentIndex, "\n\n")
	if err != nil {
		return invalidResponsesEventFailure(err)
	}
	s.enqueueResponsesEvent(delta)
	return nil
}

func (s *openAIResponsesStream) startResponsesTextSlot(index int, itemID, phase string) *responsesFailureSpec {
	if !utf8.ValidString(itemID) || strings.TrimSpace(itemID) == "" {
		return invalidResponsesEventFailure(fmt.Errorf("output message at index %d has no valid id", index))
	}
	if failure := validateResponsesMessagePhase(phase); failure != nil {
		return failure
	}
	if s.sawFinalAnswer {
		return invalidResponsesEventFailure(errors.New("output message arrived after a final_answer message"))
	}
	if _, exists := s.slots[index]; exists {
		return invalidResponsesEventFailure(fmt.Errorf("output index %d was added twice", index))
	}
	slot := &responsesTextSlot{contentIndex: s.nextContentIndex, itemID: itemID, phase: phase}
	s.slots[index] = slot
	if phase == "final_answer" {
		s.sawFinalAnswer = true
	}
	start, err := llm.NewTextStartEvent(slot.contentIndex)
	if err != nil {
		return invalidResponsesEventFailure(err)
	}
	s.enqueueResponsesEvent(start)
	return nil
}

func (s *openAIResponsesStream) startResponsesToolSlot(index int, itemID, callID, name, arguments string) *responsesFailureSpec {
	if !utf8.ValidString(itemID) || strings.TrimSpace(itemID) == "" ||
		!utf8.ValidString(callID) || strings.TrimSpace(callID) == "" ||
		!utf8.ValidString(name) || strings.TrimSpace(name) == "" {
		return invalidResponsesEventFailure(fmt.Errorf("function call at index %d has invalid identity", index))
	}
	if !utf8.ValidString(arguments) {
		return invalidResponsesEventFailure(errors.New("function call arguments are not valid UTF-8"))
	}
	id := callID + "|" + itemID
	slot := &responsesToolSlot{contentIndex: s.nextContentIndex, itemID: itemID, callID: callID, name: name}
	s.toolSlots[index] = slot
	start, err := llm.NewToolCallStartEvent(slot.contentIndex, id, name)
	if err != nil {
		return invalidResponsesEventFailure(err)
	}
	s.enqueueResponsesEvent(start)
	if arguments != "" {
		slot.arguments = append(slot.arguments, arguments...)
		delta, err := llm.NewToolCallDeltaEvent(slot.contentIndex, []byte(arguments))
		if err != nil {
			return invalidResponsesEventFailure(err)
		}
		s.enqueueResponsesEvent(delta)
	}
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
	s.enqueueResponsesEvent(delta)
	return nil
}

func (s *openAIResponsesStream) addResponsesToolDelta(event responsesOutputEvent) *responsesFailureSpec {
	index, failure := requireResponsesOutputIndex(event.OutputIndex)
	if failure != nil {
		return failure
	}
	slot := s.toolSlots[index]
	if slot == nil || slot.argumentsDone {
		return invalidResponsesEventFailure(fmt.Errorf("function call delta has no open function call at index %d", index))
	}
	if event.ItemID != "" && event.ItemID != slot.itemID {
		return invalidResponsesEventFailure(fmt.Errorf("function call delta item id %q does not match %q", event.ItemID, slot.itemID))
	}
	if !utf8.ValidString(event.Delta) {
		return invalidResponsesEventFailure(errors.New("function call arguments delta is not valid UTF-8"))
	}
	slot.arguments = append(slot.arguments, event.Delta...)
	delta, err := llm.NewToolCallDeltaEvent(slot.contentIndex, []byte(event.Delta))
	if err != nil {
		return invalidResponsesEventFailure(err)
	}
	s.enqueueResponsesEvent(delta)
	return nil
}

func (s *openAIResponsesStream) finishResponsesToolArguments(event responsesOutputEvent) *responsesFailureSpec {
	index, failure := requireResponsesOutputIndex(event.OutputIndex)
	if failure != nil {
		return failure
	}
	slot := s.toolSlots[index]
	if slot == nil || slot.argumentsDone {
		return invalidResponsesEventFailure(fmt.Errorf("function call arguments done has no open function call at index %d", index))
	}
	if event.ItemID != "" && event.ItemID != slot.itemID {
		return invalidResponsesEventFailure(fmt.Errorf("function call done item id %q does not match %q", event.ItemID, slot.itemID))
	}
	if !utf8.ValidString(event.Arguments) || !strings.HasPrefix(event.Arguments, string(slot.arguments)) {
		return invalidResponsesEventFailure(errors.New("completed function arguments do not match streamed deltas"))
	}
	if suffix := event.Arguments[len(slot.arguments):]; suffix != "" {
		slot.arguments = append(slot.arguments, suffix...)
		delta, err := llm.NewToolCallDeltaEvent(slot.contentIndex, []byte(suffix))
		if err != nil {
			return invalidResponsesEventFailure(err)
		}
		s.enqueueResponsesEvent(delta)
	}
	call, err := llm.NewToolCallBlock(slot.callID+"|"+slot.itemID, slot.name, slot.arguments)
	if err != nil {
		return invalidResponsesEventFailure(fmt.Errorf("completed function call is invalid: %w", err))
	}
	end, err := llm.NewToolCallEndEvent(slot.contentIndex, call)
	if err != nil {
		return invalidResponsesEventFailure(err)
	}
	s.enqueueResponsesEvent(end)
	slot.argumentsDone = true
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
	if index != s.nextOutputIndex {
		return invalidResponsesEventFailure(fmt.Errorf("completed output item index %d, want %d", index, s.nextOutputIndex))
	}
	if s.sawFinalAnswer && s.slots[index] == nil && s.toolSlots[index] == nil && s.reasoningSlots[index] == nil {
		return invalidResponsesEventFailure(errors.New("output item arrived after a final_answer message"))
	}
	switch event.Item.Type {
	case "message":
		if failure := validateResponsesMessagePhase(event.Item.Phase); failure != nil {
			return failure
		}
		if event.Item.Role != "assistant" {
			return invalidResponsesEventFailure(fmt.Errorf("completed output message role is %q", event.Item.Role))
		}
		slot := s.slots[index]
		if slot == nil {
			if len(s.slots) != 0 || len(s.toolSlots) != 0 || len(s.reasoningSlots) != 0 {
				return invalidResponsesEventFailure(fmt.Errorf("completed message at index %d does not match the open output item", index))
			}
			if failure := s.startResponsesTextSlot(index, event.Item.ID, event.Item.Phase); failure != nil {
				return failure
			}
			slot = s.slots[index]
		} else if event.Item.ID != "" && event.Item.ID != slot.itemID {
			return invalidResponsesEventFailure(fmt.Errorf("completed item id %q does not match %q", event.Item.ID, slot.itemID))
		}
		if event.Item.Phase != "" {
			if slot.phase != "" && event.Item.Phase != slot.phase {
				return invalidResponsesEventFailure(fmt.Errorf(
					"completed message phase %q does not match %q",
					event.Item.Phase,
					slot.phase,
				))
			}
			if slot.phase == "" {
				slot.phase = event.Item.Phase
				if slot.phase == "final_answer" {
					s.sawFinalAnswer = true
				}
			}
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
			s.enqueueResponsesEvent(delta)
		}
		signature, err := encodeResponsesTextSignature(slot.itemID, slot.phase)
		if err != nil {
			return invalidResponsesEventFailure(err)
		}
		end, err := llm.NewTextEndEventWithSignature(slot.contentIndex, finalText, signature)
		if err != nil {
			return invalidResponsesEventFailure(err)
		}
		s.enqueueResponsesEvent(end)
		delete(s.slots, index)
		s.nextContentIndex++
		s.completedOutputs[index] = struct{}{}
		s.completedItemIDs[index] = slot.itemID
		s.completedPhases[index] = slot.phase
		s.nextOutputIndex++

	case "function_call":
		slot := s.toolSlots[index]
		if slot == nil || !slot.argumentsDone {
			return invalidResponsesEventFailure(fmt.Errorf("function call at index %d ended before arguments were complete", index))
		}
		if event.Item.ID != "" && event.Item.ID != slot.itemID ||
			event.Item.CallID != "" && event.Item.CallID != slot.callID ||
			event.Item.Name != "" && event.Item.Name != slot.name ||
			event.Item.Arguments != "" && event.Item.Arguments != string(slot.arguments) {
			return invalidResponsesEventFailure(fmt.Errorf("completed function call at index %d does not match start", index))
		}
		delete(s.toolSlots, index)
		s.nextContentIndex++
		s.completedOutputs[index] = struct{}{}
		s.completedItemIDs[index] = slot.itemID
		s.nextOutputIndex++
		s.sawFunctionCall = true
	case "reasoning":
		slot := s.reasoningSlots[index]
		if slot == nil {
			if len(s.slots) != 0 || len(s.toolSlots) != 0 || len(s.reasoningSlots) != 0 {
				return invalidResponsesEventFailure(fmt.Errorf("completed reasoning at index %d does not match the open output item", index))
			}
			if failure := s.startResponsesReasoningSlot(index, event.Item.ID); failure != nil {
				return failure
			}
			slot = s.reasoningSlots[index]
		} else if event.Item.ID != "" && event.Item.ID != slot.itemID {
			return invalidResponsesEventFailure(errors.New("reasoning item id mismatch"))
		}
		text := responsesReasoningText(event.Item)
		if text != "" {
			partial := slot.text.String()
			if !strings.HasPrefix(text, partial) {
				return invalidResponsesEventFailure(errors.New("reasoning content does not match deltas"))
			}
			if suffix := text[len(partial):]; suffix != "" {
				slot.text.WriteString(suffix)
				d, err := llm.NewThinkingDeltaEvent(slot.contentIndex, suffix)
				if err != nil {
					return invalidResponsesEventFailure(err)
				}
				s.enqueueResponsesEvent(d)
			}
		}
		completedReasoning := &responsesCompletedReasoning{
			contentIndex:     slot.contentIndex,
			itemID:           slot.itemID,
			text:             slot.text.String(),
			encryptedContent: event.Item.EncryptedContent,
		}
		if event.Item.EncryptedContent == "" {
			completedReasoning.plaintextContent = slot.text.String()
			s.deferResponsesReasoningEnd(index, completedReasoning)
		} else {
			signature, err := encodeResponsesReasoningSignature(slot.itemID, event.Item.EncryptedContent, "", slot.text.String())
			if err != nil {
				return invalidResponsesEventFailure(err)
			}
			block, err := llm.NewThinkingBlockWithSignature(slot.text.String(), signature, false)
			if err != nil {
				return invalidResponsesEventFailure(err)
			}
			end, err := llm.NewThinkingEndEvent(slot.contentIndex, block)
			if err != nil {
				return invalidResponsesEventFailure(err)
			}
			s.enqueueResponsesEvent(end)
		}
		delete(s.reasoningSlots, index)
		s.nextContentIndex++
		s.completedOutputs[index] = struct{}{}
		s.completedItemIDs[index] = slot.itemID
		s.nextOutputIndex++
	case "custom_tool_call":
		return unsupportedResponsesOutputFailure(event.Item.Type)
	default:
		return unsupportedResponsesOutputFailure(event.Item.Type)
	}
	return nil
}

func validateResponsesMessagePhase(phase string) *responsesFailureSpec {
	if !utf8.ValidString(phase) {
		return invalidResponsesEventFailure(errors.New("message phase is not valid UTF-8"))
	}
	switch phase {
	case "", "commentary", "final_answer":
		return nil
	default:
		return invalidResponsesEventFailure(fmt.Errorf("unknown message phase %q", phase))
	}
}

func responsesReasoningText(item responsesOutputItem) string {
	parts := make([]string, 0, len(item.Summary)+len(item.Content))
	for _, value := range item.Summary {
		if value.Text != "" {
			parts = append(parts, value.Text)
		}
	}
	if len(parts) == 0 {
		for _, value := range item.Content {
			if value.Text != "" {
				parts = append(parts, value.Text)
			}
		}
	}
	if len(parts) != 0 {
		return strings.Join(parts, "\n\n")
	}
	return item.PlaintextContent
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
	type terminalReasoning struct {
		index            int
		encryptedContent string
	}
	terminalReasoningByID := make(map[string]terminalReasoning)
	for outputIndex, item := range event.Response.Output {
		if item.ID != "" {
			expectedID, completed := s.completedItemIDs[outputIndex]
			if !completed || item.ID != expectedID {
				return invalidResponsesEventFailure(fmt.Errorf(
					"terminal output item id %q at index %d does not match completed item",
					item.ID,
					outputIndex,
				))
			}
		}
		switch item.Type {
		case "message":
			if failure := validateResponsesMessagePhase(item.Phase); failure != nil {
				return failure
			}
			if item.Role != "" && item.Role != "assistant" {
				return invalidResponsesEventFailure(fmt.Errorf(
					"terminal output message at index %d has role %q",
					outputIndex,
					item.Role,
				))
			}
			if _, completed := s.completedOutputs[outputIndex]; !completed {
				return invalidResponsesEventFailure(fmt.Errorf(
					"terminal output message at index %d was not completed by output_item events",
					outputIndex,
				))
			}
			if item.Phase != "" && item.Phase != s.completedPhases[outputIndex] {
				return invalidResponsesEventFailure(fmt.Errorf(
					"terminal output message phase %q at index %d does not match %q",
					item.Phase,
					outputIndex,
					s.completedPhases[outputIndex],
				))
			}
			if len(item.Content) != 0 {
				if _, failure := responsesOutputItemText(item); failure != nil {
					return failure
				}
			}
		case "function_call":
			if _, completed := s.completedOutputs[outputIndex]; !completed {
				return invalidResponsesEventFailure(fmt.Errorf("terminal function call at index %d was not completed by output_item events", outputIndex))
			}
		case "reasoning":
			if _, completed := s.completedOutputs[outputIndex]; !completed {
				return invalidResponsesEventFailure(fmt.Errorf("terminal reasoning item at index %d was not completed", outputIndex))
			}
			if item.ID != "" {
				if previous, duplicate := terminalReasoningByID[item.ID]; duplicate {
					return invalidResponsesEventFailure(fmt.Errorf(
						"terminal reasoning item id %q is duplicated at indexes %d and %d",
						item.ID,
						previous.index,
						outputIndex,
					))
				}
				terminalReasoningByID[item.ID] = terminalReasoning{
					index:            outputIndex,
					encryptedContent: item.EncryptedContent,
				}
			}
		case "custom_tool_call":
			return unsupportedResponsesOutputFailure(item.Type)
		default:
			return unsupportedResponsesOutputFailure(item.Type)
		}
	}
	for _, pending := range s.pendingReasoning {
		if terminal, ok := terminalReasoningByID[pending.itemID]; ok {
			pending.encryptedContent = terminal.encryptedContent
			if terminal.encryptedContent != "" {
				pending.plaintextContent = ""
			}
		}
	}
	if len(s.slots) != 0 || len(s.toolSlots) != 0 || len(s.reasoningSlots) != 0 {
		return invalidResponsesEventFailure(errors.New("terminal response arrived with an open output item"))
	}
	if err := s.flushResponsesDeferredEvents(); err != nil {
		return invalidResponsesEventFailure(fmt.Errorf("seal deferred reasoning: %w", err))
	}
	usage, failure := normalizeResponsesUsage(event.Response.Usage)
	if failure != nil {
		return failure
	}
	if event.Response.Usage != nil {
		var costErr error
		usage, costErr = usage.WithCost(s.model.CalculateCost(usage))
		if costErr != nil { return invalidResponsesEventFailure(fmt.Errorf("calculate token cost: %w", costErr)) }
	}
	// A completed function call always produces a tool-use terminal even when
	// the server reports the generic completed status.
	if reason == llm.FinishStop && s.sawFunctionCall {
		reason = llm.FinishToolUse
	}
	response := &llm.AssistantResponseMetadata{ResponseID: event.Response.ID, RawStopReason: wantStatus}
	provenance := &llm.AssistantProvenance{Provider: s.model.Provider(), API: s.model.API(), Model: s.model.ID()}
	done, err := llm.NewDoneEventWithMetadata(reason, usage, s.timestamp, provenance, response)
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
