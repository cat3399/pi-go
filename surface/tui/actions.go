package tui

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

type inputActionKind uint8

const (
	inputActionDispatch inputActionKind = iota + 1
	inputActionQuit
	inputActionHelp
	inputActionNewSession
	inputActionOpenSession
)

type inputAction struct {
	kind      inputActionKind
	command   application.Command
	sessionID string
}

func planInput(text string, state application.State, followUp bool) (inputAction, error) {
	return planRichInput(text, nil, state, followUp)
}

func planRichInput(text string, images []llm.ImageBlock, state application.State, followUp bool) (inputAction, error) {
	text = strings.TrimSpace(text)
	if text == "" && len(images) == 0 {
		return inputAction{}, errors.New("message is empty")
	}
	// A draft with attachments is always a prompt. Interpreting its text as a
	// local slash or shell command would silently discard the attached content.
	if len(images) != 0 {
		return promptInputAction(text, images, state, followUp), nil
	}
	if strings.HasPrefix(text, "!!") {
		command := strings.TrimSpace(strings.TrimPrefix(text, "!!"))
		if command == "" {
			return inputAction{}, errors.New("bash command is empty")
		}
		return inputAction{kind: inputActionDispatch, command: application.BashCommand{
			Command: command, ExcludeFromContext: true,
		}}, nil
	}
	if strings.HasPrefix(text, "!") {
		command := strings.TrimSpace(strings.TrimPrefix(text, "!"))
		if command == "" {
			return inputAction{}, errors.New("bash command is empty")
		}
		return inputAction{kind: inputActionDispatch, command: application.BashCommand{Command: command}}, nil
	}

	name, argument, slash := splitSlashCommand(text)
	if slash {
		switch name {
		case "quit", "exit":
			return inputAction{kind: inputActionQuit}, nil
		case "help", "hotkeys":
			return inputAction{kind: inputActionHelp}, nil
		case "new":
			if argument != "" {
				return inputAction{}, errors.New("usage: /new")
			}
			return inputAction{kind: inputActionNewSession}, nil
		case "resume":
			if argument == "" {
				return inputAction{}, errors.New("usage: /resume <session-id>")
			}
			return inputAction{kind: inputActionOpenSession, sessionID: argument}, nil
		case "abort":
			return inputAction{kind: inputActionDispatch, command: application.AbortCommand{}}, nil
		case "reload":
			return inputAction{kind: inputActionDispatch, command: application.ReloadCommand{}}, nil
		case "compact":
			return inputAction{kind: inputActionDispatch, command: application.CompactCommand{CustomInstructions: argument}}, nil
		case "clear-queue", "dequeue":
			return inputAction{kind: inputActionDispatch, command: application.ClearQueueCommand{}}, nil
		case "name":
			if argument == "" {
				return inputAction{}, errors.New("usage: /name <session-name>")
			}
			return inputAction{kind: inputActionDispatch, command: application.SetSessionNameCommand{Name: argument}}, nil
		case "model":
			providerID, modelID, ok := strings.Cut(argument, "/")
			if !ok || strings.TrimSpace(providerID) == "" || strings.TrimSpace(modelID) == "" {
				return inputAction{}, errors.New("usage: /model <provider>/<model-id>")
			}
			return inputAction{kind: inputActionDispatch, command: application.SetModelCommand{
				Provider: strings.TrimSpace(providerID), ModelID: strings.TrimSpace(modelID),
			}}, nil
		case "thinking":
			level := provider.ThinkingLevel(strings.TrimSpace(argument))
			if !level.Valid() {
				return inputAction{}, fmt.Errorf("invalid thinking level %q", argument)
			}
			return inputAction{kind: inputActionDispatch, command: application.SetThinkingLevelCommand{Level: level}}, nil
		case "stats":
			return inputAction{kind: inputActionDispatch, command: application.GetSessionStatsCommand{}}, nil
		case "tools":
			return inputAction{kind: inputActionDispatch, command: application.GetToolsCommand{}}, nil
		}
		// Unknown slash commands remain prompts: templates, skills, and future
		// extension commands are resolved by the transport-neutral core.
	}

	return promptInputAction(text, nil, state, followUp), nil
}

func promptInputAction(text string, images []llm.ImageBlock, state application.State, followUp bool) inputAction {
	behavior := agent.StreamingBehavior("")
	if state.IsStreaming || state.IsPromptRunning || state.IsCompacting {
		behavior = agent.StreamingSteer
		if followUp {
			behavior = agent.StreamingFollowUp
		}
	}
	return inputAction{kind: inputActionDispatch, command: application.PromptCommand{
		Message: text, Images: append([]llm.ImageBlock(nil), images...),
		StreamingBehavior: behavior, Source: agent.InputInteractive,
	}}
}

func splitSlashCommand(value string) (name, argument string, ok bool) {
	if !strings.HasPrefix(value, "/") || len(value) == 1 {
		return "", "", false
	}
	parts := strings.SplitN(value[1:], " ", 2)
	name = strings.ToLower(strings.TrimSpace(parts[0]))
	if len(parts) == 2 {
		argument = strings.TrimSpace(parts[1])
	}
	return name, argument, name != ""
}
