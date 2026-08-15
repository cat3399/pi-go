package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
)

type subscriptionReadyMsg struct {
	subscription *application.EventSubscription
	generation   uint64
	err          error
}

type applicationEventMsg struct {
	event      application.Event
	generation uint64
	ok         bool
}

type reconnectEventsMsg struct{ generation uint64 }

type retrySnapshotMsg struct{ sessionGeneration uint64 }

type statusExpiryMsg struct{ generation uint64 }

type commandsLoadedMsg struct {
	sessionID         string
	sessionGeneration uint64
	request           uint64
	commands          []application.SlashCommandInfo
	err               error
}

type stateLoadedMsg struct {
	sessionID         string
	sessionGeneration uint64
	request           uint64
	state             application.State
	active            bool
	err               error
}

type snapshotLoadedMsg struct {
	sessionID         string
	sessionGeneration uint64
	request           uint64
	snapshot          application.SessionSnapshot
	err               error
}

type commandFinishedMsg struct {
	sessionID         string
	sessionGeneration uint64
	request           uint64
	command           application.Command
	result            application.CommandResult
	draft             string
	draftImages       []llm.ImageBlock
	err               error
}

type sessionOpenedMsg struct {
	sourceGeneration uint64
	request          uint64
	state            application.State
	snapshot         application.SessionSnapshot
	err              error
}

func subscribeEventsCmd(api application.API, after, generation uint64) tea.Cmd {
	return func() tea.Msg {
		subscription, err := api.SubscribeEvents(after)
		return subscriptionReadyMsg{subscription: subscription, generation: generation, err: err}
	}
}

func waitApplicationEventCmd(subscription *application.EventSubscription, generation uint64) tea.Cmd {
	if subscription == nil {
		return nil
	}
	return func() tea.Msg {
		event, ok := <-subscription.Events
		return applicationEventMsg{event: event, generation: generation, ok: ok}
	}
}

func reconnectEventsCmd(generation uint64) tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg {
		return reconnectEventsMsg{generation: generation}
	})
}

func retrySnapshotCmd(sessionGeneration uint64) tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg {
		return retrySnapshotMsg{sessionGeneration: sessionGeneration}
	})
}

func expireStatusCmd(generation uint64) tea.Cmd {
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg {
		return statusExpiryMsg{generation: generation}
	})
}

func loadStateCmd(api application.API, sessionID string, sessionGeneration, request uint64) tea.Cmd {
	return func() tea.Msg {
		state, active, err := api.LiveState(sessionID)
		return stateLoadedMsg{
			sessionID: sessionID, sessionGeneration: sessionGeneration, request: request,
			state: state, active: active, err: err,
		}
	}
}

func loadSnapshotCmd(api application.API, sessionID string, sessionGeneration, request uint64) tea.Cmd {
	return func() tea.Msg {
		snapshot, err := api.SnapshotSession(sessionID, "")
		return snapshotLoadedMsg{
			sessionID: sessionID, sessionGeneration: sessionGeneration, request: request,
			snapshot: snapshot, err: err,
		}
	}
}

func loadCommandsCmd(ctx context.Context, api application.API, sessionID string, sessionGeneration, request uint64) tea.Cmd {
	return func() tea.Msg {
		result, err := api.Dispatch(ctx, sessionID, application.GetCommandsCommand{})
		message := commandsLoadedMsg{
			sessionID: sessionID, sessionGeneration: sessionGeneration, request: request, err: err,
		}
		if commands, ok := result.(application.GetCommandsResult); ok {
			message.commands = append([]application.SlashCommandInfo(nil), commands.Commands...)
		} else if err == nil {
			message.err = application.ErrInvalidCommand
		}
		return message
	}
}

func dispatchCommandCmd(
	ctx context.Context,
	api application.API,
	sessionID string,
	sessionGeneration, request uint64,
	command application.Command,
	draft string,
	draftImages []llm.ImageBlock,
) tea.Cmd {
	return func() tea.Msg {
		result, err := api.Dispatch(ctx, sessionID, command)
		return commandFinishedMsg{
			sessionID: sessionID, sessionGeneration: sessionGeneration, request: request,
			command: command, result: result, draft: draft,
			draftImages: append([]llm.ImageBlock(nil), draftImages...), err: err,
		}
	}
}

func createSessionCmd(ctx context.Context, api application.API, current application.State, sourceGeneration, request uint64) tea.Cmd {
	return func() tea.Msg {
		options := application.NewSessionOptions{CWD: current.CWD}
		if options.CWD == "" {
			options.CWD = api.DefaultCWD()
		}
		if current.HasModel {
			options.Provider = current.Model.Provider()
			options.ModelID = current.Model.ID()
		}
		if current.ThinkingLevel.Valid() {
			level := current.ThinkingLevel
			options.ThinkingLevel = &level
		}
		state, err := api.NewSession(ctx, options)
		if err != nil {
			return sessionOpenedMsg{sourceGeneration: sourceGeneration, request: request, err: err}
		}
		snapshot, err := api.SnapshotSession(state.SessionID, "")
		return sessionOpenedMsg{
			sourceGeneration: sourceGeneration, request: request,
			state: state, snapshot: snapshot, err: err,
		}
	}
}

func openSessionCmd(ctx context.Context, api application.API, sessionID string, sourceGeneration, request uint64) tea.Cmd {
	return func() tea.Msg {
		result, err := api.Dispatch(ctx, sessionID, application.GetStateCommand{})
		if err != nil {
			return sessionOpenedMsg{sourceGeneration: sourceGeneration, request: request, err: err}
		}
		stateResult, ok := result.(application.GetStateResult)
		if !ok {
			return sessionOpenedMsg{sourceGeneration: sourceGeneration, request: request, err: application.ErrInvalidCommand}
		}
		snapshot, err := api.SnapshotSession(sessionID, "")
		return sessionOpenedMsg{
			sourceGeneration: sourceGeneration, request: request,
			state: stateResult.State, snapshot: snapshot, err: err,
		}
	}
}
