package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
)

const (
	defaultSessionTitleTimeout = 90 * time.Second
	maxSessionTitleRunes       = 80
)

const sessionTitlePrompt = `Create a concise title for this session based on the conversation above.

Requirements:
- Match the primary language used by the user.
- Describe the user's concrete goal or the outcome, not the act of chatting.
- Use 4-12 words for space-separated languages, or 8-24 characters for CJK text when practical.
- Do not call any tools.
- Return only the title as plain text, with no quotes, label, markdown, or explanation.`

type SessionTitleUsage struct {
	Input      uint64
	Output     uint64
	CacheRead  uint64
	CacheWrite uint64
	Total      uint64
}

type GeneratedSessionTitle struct {
	Title string
	Usage SessionTitleUsage
}

type sessionTitleToolExecutor struct{}

func (sessionTitleToolExecutor) Name() string         { return "session-title-disabled-tools" }
func (sessionTitleToolExecutor) Supports(string) bool { return true }
func (sessionTitleToolExecutor) Execute(context.Context, string, []byte, func(ToolUpdate)) (ToolOutput, error) {
	return ToolOutput{}, errors.New("tools cannot be executed while generating a session title")
}
func (sessionTitleToolExecutor) ExecuteNamed(context.Context, string, string, []byte, func(ToolUpdate)) (ToolOutput, error) {
	return ToolOutput{}, errors.New("tools cannot be executed while generating a session title")
}

// GenerateSessionTitle uses a temporary, non-persistent Agent whose
// provider-facing state mirrors this session. Tool schemas remain visible so
// request assembly stays identical, while the executor is replaced with a
// rejecting implementation and no title-run messages can enter the transcript.
func (s *AgentSession) GenerateSessionTitle(ctx context.Context) (GeneratedSessionTitle, error) {
	if s == nil || s.loop == nil {
		return GeneratedSessionTitle{}, fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.WaitForIdle(ctx); err != nil {
		return GeneratedSessionTitle{}, err
	}
	if err := s.rejectIfClosed(); err != nil {
		return GeneratedSessionTitle{}, err
	}

	state := s.loop.State()
	messages, err := sanitizeSessionTitleMessages(state.Messages())
	if err != nil {
		return GeneratedSessionTitle{}, err
	}
	if !containsSessionTitleUserMessage(messages) {
		return GeneratedSessionTitle{}, errors.New("the session has no user messages to name")
	}
	messages, continues := appendSessionTitleRequest(messages)

	policy := s.loop.config
	var titleTools ToolExecutor
	if len(state.Tools()) != 0 {
		titleTools = sessionTitleToolExecutor{}
	}
	prepareTurn := policy.prepareTurn
	temporary, err := New(Config{
		Provider: policy.provider, InitialMessages: messages,
		Model: state.Model(), ThinkingLevel: state.ThinkingLevel(), SystemPrompt: state.SystemPrompt(),
		Stream: policy.stream, Tool: titleTools, Tools: state.Tools(), ToolExecution: policy.toolExecution,
		BeforeToolCall: policy.beforeToolCall, AfterToolCall: policy.afterToolCall,
		TransformContext: policy.transformContext, TransformAgentContext: policy.transformAgentContext,
		ConvertToLLM: policy.convertToLLM, GetAPIKey: policy.getAPIKey,
		MessageEnd: policy.messageEnd, PrepareNextTurn: policy.prepareNextTurn,
		SteeringMode: policy.steeringMode, FollowUpMode: policy.followUpMode, Now: policy.now,
		PrepareTurn: func(turnCtx context.Context, turn TurnContext) (TurnSnapshot, error) {
			if prepareTurn == nil {
				return TurnSnapshot{
					Model: state.Model(), ThinkingLevel: state.ThinkingLevel(), SystemPrompt: state.SystemPrompt(),
					Tool: titleTools, Tools: state.Tools(), Stream: policy.stream,
				}, nil
			}
			snapshot, prepareErr := prepareTurn(turnCtx, turn)
			if prepareErr != nil {
				return TurnSnapshot{}, prepareErr
			}
			snapshot.Tool = titleTools
			snapshot.Tools = state.Tools()
			return snapshot, nil
		},
	})
	if err != nil {
		return GeneratedSessionTitle{}, err
	}

	runCtx, cancel := context.WithTimeout(ctx, defaultSessionTitleTimeout)
	defer cancel()
	var result Result
	if continues {
		result, err = temporary.Continue(runCtx)
	} else {
		result, err = temporary.Run(runCtx, sessionTitlePrompt)
	}
	if err != nil {
		_ = temporary.Abort(context.Background())
		if errors.Is(context.Cause(runCtx), context.DeadlineExceeded) {
			return GeneratedSessionTitle{}, errors.New("session title generation timed out")
		}
		return GeneratedSessionTitle{}, err
	}
	terminal, ok := result.Terminal()
	if !ok {
		return GeneratedSessionTitle{}, errors.New("the model did not return a session title")
	}
	if terminal.FinishReason() == llm.FinishError {
		if failure, isFailure := terminal.(llm.AssistantFailureMessage); isFailure && failure.ErrorMessage() != "" {
			return GeneratedSessionTitle{}, errors.New(failure.ErrorMessage())
		}
		return GeneratedSessionTitle{}, errors.New("the title model request failed")
	}
	if terminal.FinishReason() == llm.FinishAborted {
		if errors.Is(context.Cause(runCtx), context.DeadlineExceeded) {
			return GeneratedSessionTitle{}, errors.New("session title generation timed out")
		}
		return GeneratedSessionTitle{}, errors.New("session title generation was aborted")
	}
	var text strings.Builder
	for _, block := range terminal.Blocks() {
		if value, isText := block.(llm.TextBlock); isText {
			if text.Len() != 0 {
				text.WriteByte('\n')
			}
			text.WriteString(value.Text())
		}
	}
	title, err := ParseGeneratedSessionTitle(text.String())
	if err != nil {
		return GeneratedSessionTitle{}, err
	}
	usage := terminal.Usage()
	return GeneratedSessionTitle{
		Title: title,
		Usage: SessionTitleUsage{
			Input: usage.Input(), Output: usage.Output(), CacheRead: usage.CacheRead(),
			CacheWrite: usage.CacheWrite(), Total: usage.TotalTokens(),
		},
	}, nil
}

func containsSessionTitleUserMessage(messages []agentmsg.Message) bool {
	for _, message := range messages {
		if message != nil && message.Role() == agentmsg.RoleUser {
			return true
		}
	}
	return false
}

func appendSessionTitleRequest(messages []agentmsg.Message) ([]agentmsg.Message, bool) {
	if len(messages) == 0 || messages[len(messages)-1].Role() != agentmsg.RoleUser {
		return messages, false
	}
	wrapper, ok := messages[len(messages)-1].(agentmsg.LLM)
	if !ok {
		return messages, false
	}
	var replacement llm.ConversationMessage
	switch message := wrapper.Conversation().(type) {
	case llm.UserTextMessage:
		blocks := message.Content()
		if len(blocks) == 0 {
			return messages, false
		}
		request, err := llm.NewTextBlock(blocks[len(blocks)-1].Text() + "\n\n" + sessionTitlePrompt)
		if err != nil {
			return messages, false
		}
		blocks[len(blocks)-1] = request
		replacement, err = llm.NewUserTextBlocksMessage(blocks, message.Timestamp())
		if err != nil {
			return messages, false
		}
	case llm.UserContentMessage:
		blocks := message.Content()
		request, err := llm.NewTextBlock(sessionTitlePrompt)
		if err != nil {
			return messages, false
		}
		blocks = append(blocks, request)
		replacement, err = llm.NewUserContentMessage(blocks, message.Timestamp())
		if err != nil {
			return messages, false
		}
	default:
		return messages, false
	}
	wrappedReplacement, err := agentmsg.NewLLM(replacement)
	if err != nil {
		return messages, false
	}
	result := agentmsg.Clone(messages)
	result[len(result)-1] = wrappedReplacement
	return result, true
}

// sanitizeSessionTitleMessages removes incomplete tool batches so a title run
// cannot send an invalid provider transcript. Completed messages retain their
// original immutable values and metadata.
func sanitizeSessionTitleMessages(messages []agentmsg.Message) ([]agentmsg.Message, error) {
	result := make([]agentmsg.Message, 0, len(messages))
	var expected map[string]struct{}
	for index, message := range messages {
		if message == nil {
			continue
		}
		if message.Role() == agentmsg.RoleAssistant {
			following := make(map[string]struct{})
			for next := index + 1; next < len(messages); next++ {
				id, ok := sessionTitleToolResultID(messages[next])
				if !ok {
					break
				}
				following[id] = struct{}{}
			}
			filtered, completed, filterErr := filterSessionTitleAssistant(message, following)
			if filterErr != nil {
				return nil, filterErr
			}
			expected = completed
			if filtered != nil {
				result = append(result, filtered)
			}
			continue
		}
		if message.Role() == agentmsg.RoleToolResult {
			id, ok := sessionTitleToolResultID(message)
			if !ok {
				continue
			}
			if _, ok := expected[id]; !ok {
				continue
			}
			delete(expected, id)
			result = append(result, agentmsg.CloneOne(message))
			continue
		}
		expected = nil
		result = append(result, agentmsg.CloneOne(message))
	}
	return result, nil
}

func filterSessionTitleAssistant(message agentmsg.Message, following map[string]struct{}) (agentmsg.Message, map[string]struct{}, error) {
	expected := make(map[string]struct{})
	wrapper, ok := message.(agentmsg.LLM)
	if !ok {
		return agentmsg.CloneOne(message), expected, nil
	}
	assistant, ok := wrapper.Conversation().(llm.AssistantTerminal)
	if !ok {
		return agentmsg.CloneOne(message), expected, nil
	}
	blocks := assistant.Blocks()
	filtered := make([]llm.AssistantBlock, 0, len(blocks))
	changed := false
	for _, block := range blocks {
		call, isCall := block.(llm.ToolCallBlock)
		if !isCall {
			filtered = append(filtered, block)
			continue
		}
		if _, complete := following[call.ID()]; !complete {
			changed = true
			continue
		}
		expected[call.ID()] = struct{}{}
		filtered = append(filtered, block)
	}
	if !changed {
		return agentmsg.CloneOne(message), expected, nil
	}
	if len(filtered) == 0 {
		return nil, expected, nil
	}
	response, hasResponse := assistant.ResponseMetadata()
	var responsePointer *llm.AssistantResponseMetadata
	if hasResponse {
		responsePointer = &response
	}
	var rebuilt llm.AssistantTerminal
	var err error
	if failure, isFailure := assistant.(llm.AssistantFailureMessage); isFailure {
		rebuilt, err = llm.NewAssistantFailureMessageWithBlocksAndMetadata(
			filtered, failure.FinishReason(), failure.Failure(), failure.Usage(), failure.Timestamp(),
			failure.AssistantProvenance(), responsePointer, failure.Diagnostics(),
		)
	} else if len(expected) != 0 {
		rebuilt, err = llm.NewAssistantToolUseMessageWithFinishAndMetadata(
			filtered, assistant.FinishReason(), assistant.Usage(), assistant.Timestamp(),
			assistant.AssistantProvenance(), responsePointer, assistant.Diagnostics(),
		)
	} else {
		finish := assistant.FinishReason()
		if finish != llm.FinishStop && finish != llm.FinishLength {
			finish = llm.FinishStop
		}
		texts := make([]llm.TextBlock, 0, len(filtered))
		onlyText := true
		for _, block := range filtered {
			text, isText := block.(llm.TextBlock)
			if !isText {
				onlyText = false
				break
			}
			texts = append(texts, text)
		}
		if onlyText {
			rebuilt, err = llm.NewAssistantTextMessageWithMetadata(
				texts, finish, assistant.Usage(), assistant.Timestamp(), assistant.AssistantProvenance(),
				responsePointer, assistant.Diagnostics(),
			)
		} else {
			rebuilt, err = llm.NewAssistantRichMessageWithMetadata(
				filtered, finish, assistant.Usage(), assistant.Timestamp(), assistant.AssistantProvenance(),
				responsePointer, assistant.Diagnostics(),
			)
		}
	}
	if err != nil {
		return nil, nil, fmt.Errorf("sanitize session title context: %w", err)
	}
	result, err := agentmsg.NewLLM(rebuilt)
	if err != nil {
		return nil, nil, fmt.Errorf("sanitize session title context: %w", err)
	}
	return result, expected, nil
}

func sessionTitleToolResultID(message agentmsg.Message) (string, bool) {
	wrapper, ok := message.(agentmsg.LLM)
	if !ok {
		return "", false
	}
	switch result := wrapper.Conversation().(type) {
	case llm.ToolResultMessage:
		return result.ToolCallID(), true
	case llm.ToolResultContentMessage:
		return result.ToolCallID(), true
	default:
		return "", false
	}
}

var (
	sessionTitleFence  = regexp.MustCompile("(?is)^```(?:json|text)?\\s*(.*?)\\s*```$")
	sessionTitlePrefix = regexp.MustCompile(`(?i)^(?:session\s+title|title|标题)\s*[:：-]\s*`)
)

// ParseGeneratedSessionTitle accepts the same common model wrappers as pi-web
// and returns a single safe, bounded plain-text title.
func ParseGeneratedSessionTitle(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if fenced := sessionTitleFence.FindStringSubmatch(value); len(fenced) == 2 {
		value = strings.TrimSpace(fenced[1])
	}
	if strings.HasPrefix(value, "{") {
		var object struct {
			Title string `json:"title"`
		}
		if json.Unmarshal([]byte(value), &object) == nil && object.Title != "" {
			value = strings.TrimSpace(object.Title)
		}
	}
	if line, _, found := strings.Cut(value, "\n"); found {
		value = line
	}
	value = sessionTitlePrefix.ReplaceAllString(strings.TrimSpace(value), "")
	value = stripSessionTitleQuotes(strings.TrimSpace(value))
	value = strings.Join(strings.Fields(value), " ")
	value = strings.TrimRightFunc(strings.TrimSpace(value), func(r rune) bool {
		return r == '。' || r == '.' || r == '!'
	})
	usable := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			usable = true
			break
		}
	}
	if !usable || !utf8.ValidString(value) {
		return "", errors.New("the model did not return a usable session title")
	}
	runes := []rune(value)
	if len(runes) > maxSessionTitleRunes {
		value = strings.TrimSpace(string(runes[:maxSessionTitleRunes]))
	}
	return value, nil
}

func stripSessionTitleQuotes(value string) string {
	pairs := [][2]string{{"\"", "\""}, {"'", "'"}, {"`", "`"}, {"“", "”"}, {"「", "」"}, {"『", "』"}}
	for _, pair := range pairs {
		if strings.HasPrefix(value, pair[0]) && strings.HasSuffix(value, pair[1]) && len(value) > len(pair[0])+len(pair[1]) {
			return strings.TrimSpace(value[len(pair[0]) : len(value)-len(pair[1])])
		}
	}
	return value
}
