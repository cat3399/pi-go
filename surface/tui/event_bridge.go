package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

type clipboardImageMsg struct {
	image llm.ImageBlock
	err   error
}

type fileCompletionsLoadedMsg struct {
	generation uint64
	target     fileCompletionTarget
	result     application.FileIndexResult
	err        error
}

func readClipboardImageCmd(
	ctx context.Context,
	reader func(context.Context) (llm.ImageBlock, error),
) tea.Cmd {
	return func() tea.Msg {
		if reader == nil {
			return clipboardImageMsg{err: errors.New("clipboard image reader is unavailable")}
		}
		image, err := reader(ctx)
		return clipboardImageMsg{image: image, err: err}
	}
}

func loadFileCompletionsCmd(
	ctx context.Context,
	api application.API,
	cwd string,
	target fileCompletionTarget,
	generation uint64,
) tea.Cmd {
	return func() tea.Msg {
		result, err := api.QueryFileIndex(ctx, cwd, target.query)
		return fileCompletionsLoadedMsg{generation: generation, target: target, result: result, err: err}
	}
}

type selectorLoadedMsg struct {
	kind               selectorKind
	selectorGeneration uint64
	sessionID          string
	sessionGeneration  uint64
	models             application.ModelsSnapshot
	sessions           []application.SessionInfo
	tools              []application.ToolInfo
	providers          []application.ProviderAuthInfo
	snapshot           application.SessionSnapshot
	err                error
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
	restoreAndAbort   bool
	err               error
}

type sessionOpenedMsg struct {
	sourceGeneration uint64
	request          uint64
	state            application.State
	snapshot         application.SessionSnapshot
	err              error
	cancelled        bool
	notice           string
}

type sessionExportedMsg struct {
	path string
	err  error
}

type sessionMutationMsg struct {
	sessionGeneration uint64
	operation         string
	err               error
}

type projectTrustMsg struct {
	status  application.ProjectTrustStatus
	applied bool
	err     error
}

type providerAuthMutationMsg struct {
	generation uint64
	providerID string
	operation  string
	err        error
}

type providerOAuthStartedMsg struct {
	generation uint64
	providerID string
	login      *application.ProviderOAuthLogin
	err        error
}

type providerOAuthFinishedMsg struct {
	generation uint64
	providerID string
	login      *application.ProviderOAuthLogin
	err        error
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

func loadModelSelectorCmd(
	ctx context.Context,
	api application.API,
	cwd, sessionID string,
	sessionGeneration, selectorGeneration uint64,
	kind selectorKind,
) tea.Cmd {
	return func() tea.Msg {
		models, err := api.ListModels(ctx, cwd)
		return selectorLoadedMsg{
			kind: kind, selectorGeneration: selectorGeneration,
			sessionID: sessionID, sessionGeneration: sessionGeneration,
			models: models, err: err,
		}
	}
}

func loadSessionSelectorCmd(
	ctx context.Context,
	api application.API,
	sessionID string,
	sessionGeneration, selectorGeneration uint64,
) tea.Cmd {
	return func() tea.Msg {
		if err := context.Cause(ctx); err != nil {
			return selectorLoadedMsg{
				kind: selectorSessions, selectorGeneration: selectorGeneration,
				sessionID: sessionID, sessionGeneration: sessionGeneration, err: err,
			}
		}
		sessions, err := api.ListSessions()
		if err == nil {
			err = context.Cause(ctx)
		}
		return selectorLoadedMsg{
			kind: selectorSessions, selectorGeneration: selectorGeneration,
			sessionID: sessionID, sessionGeneration: sessionGeneration,
			sessions: append([]application.SessionInfo(nil), sessions...), err: err,
		}
	}
}

func loadToolSelectorCmd(
	ctx context.Context,
	api application.API,
	sessionID string,
	sessionGeneration, selectorGeneration uint64,
) tea.Cmd {
	return func() tea.Msg {
		result, err := api.Dispatch(ctx, sessionID, application.GetToolsCommand{})
		message := selectorLoadedMsg{
			kind: selectorTools, selectorGeneration: selectorGeneration,
			sessionID: sessionID, sessionGeneration: sessionGeneration, err: err,
		}
		if tools, ok := result.(application.GetToolsResult); ok {
			message.tools = append([]application.ToolInfo(nil), tools.Tools...)
		} else if err == nil {
			message.err = application.ErrInvalidCommand
		}
		return message
	}
}

func loadTreeSelectorCmd(
	ctx context.Context,
	api application.API,
	sessionID string,
	sessionGeneration, selectorGeneration uint64,
	kind selectorKind,
) tea.Cmd {
	return func() tea.Msg {
		if err := context.Cause(ctx); err != nil {
			return selectorLoadedMsg{
				kind: kind, selectorGeneration: selectorGeneration,
				sessionID: sessionID, sessionGeneration: sessionGeneration, err: err,
			}
		}
		snapshot, err := api.SnapshotSession(sessionID, "")
		if err == nil {
			err = context.Cause(ctx)
		}
		return selectorLoadedMsg{
			kind: kind, selectorGeneration: selectorGeneration,
			sessionID: sessionID, sessionGeneration: sessionGeneration,
			snapshot: snapshot, err: err,
		}
	}
}

func loadProviderSelectorCmd(
	ctx context.Context,
	api application.API,
	cwd, sessionID string,
	sessionGeneration, selectorGeneration uint64,
	kind selectorKind,
) tea.Cmd {
	return func() tea.Msg {
		if err := context.Cause(ctx); err != nil {
			return selectorLoadedMsg{
				kind: kind, selectorGeneration: selectorGeneration,
				sessionID: sessionID, sessionGeneration: sessionGeneration, err: err,
			}
		}
		providers, err := api.ListModelProviders(ctx, cwd)
		if err == nil {
			err = context.Cause(ctx)
		}
		return selectorLoadedMsg{
			kind: kind, selectorGeneration: selectorGeneration,
			sessionID: sessionID, sessionGeneration: sessionGeneration,
			providers: append([]application.ProviderAuthInfo(nil), providers...), err: err,
		}
	}
}

func setProviderAPIKeyCmd(
	ctx context.Context,
	api application.API,
	providerID, apiKey string,
	generation uint64,
) tea.Cmd {
	return func() tea.Msg {
		err := api.SetProviderAPIKey(ctx, providerID, apiKey)
		return providerAuthMutationMsg{
			generation: generation, providerID: providerID, operation: "Login", err: err,
		}
	}
}

func deleteProviderCredentialCmd(
	ctx context.Context,
	api application.API,
	providerID, credentialType string,
	generation uint64,
) tea.Cmd {
	return func() tea.Msg {
		err := api.DeleteProviderCredential(ctx, providerID, credentialType)
		return providerAuthMutationMsg{
			generation: generation, providerID: providerID, operation: "Logout", err: err,
		}
	}
}

func startProviderOAuthCmd(
	ctx context.Context,
	api application.API,
	providerID string,
	generation uint64,
) tea.Cmd {
	return func() tea.Msg {
		login, err := api.StartProviderOAuth(ctx, providerID)
		return providerOAuthStartedMsg{
			generation: generation, providerID: providerID, login: login, err: err,
		}
	}
}

func waitProviderOAuthCmd(
	ctx context.Context,
	login *application.ProviderOAuthLogin,
	providerID string,
	generation uint64,
) tea.Cmd {
	return func() tea.Msg {
		var err error
		if login == nil {
			err = errors.New("OAuth login did not start")
		} else {
			err = login.Wait(ctx)
		}
		return providerOAuthFinishedMsg{
			generation: generation, providerID: providerID, login: login, err: err,
		}
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

func restoreQueuedMessagesCmd(
	ctx context.Context,
	api application.API,
	sessionID string,
	sessionGeneration, request uint64,
	draft string,
	draftImages []llm.ImageBlock,
	abort bool,
) tea.Cmd {
	return func() tea.Msg {
		result, clearErr := api.Dispatch(ctx, sessionID, application.ClearQueueCommand{})
		var abortErr error
		if abort {
			_, abortErr = api.Dispatch(ctx, sessionID, application.AbortCommand{})
		}
		return commandFinishedMsg{
			sessionID: sessionID, sessionGeneration: sessionGeneration, request: request,
			command: application.ClearQueueCommand{}, result: result, draft: draft,
			draftImages: append([]llm.ImageBlock(nil), draftImages...), restoreAndAbort: abort,
			err: errors.Join(clearErr, abortErr),
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

func importSessionCmd(
	ctx context.Context,
	api application.API,
	currentID, inputPath string,
	sourceGeneration, request uint64,
) tea.Cmd {
	return func() tea.Msg {
		result, err := api.ImportSession(ctx, currentID, inputPath, "")
		if err != nil {
			return sessionOpenedMsg{sourceGeneration: sourceGeneration, request: request, err: err}
		}
		if result.Cancelled {
			return sessionOpenedMsg{sourceGeneration: sourceGeneration, request: request, cancelled: true}
		}
		snapshot, err := api.SnapshotSession(result.State.SessionID, "")
		return sessionOpenedMsg{
			sourceGeneration: sourceGeneration, request: request, state: result.State, snapshot: snapshot,
			notice: "Imported session from " + inputPath, err: err,
		}
	}
}

func exportSessionCmd(ctx context.Context, api application.API, sessionID, cwd, requestedPath string) tea.Cmd {
	return func() tea.Msg {
		var fileName string
		var data []byte
		var err error
		if strings.EqualFold(filepath.Ext(requestedPath), ".jsonl") {
			export, exportErr := api.ExportSessionJSONL(ctx, sessionID)
			fileName, data, err = export.FileName, export.JSONL, exportErr
		} else {
			export, exportErr := api.ExportSession(ctx, sessionID)
			fileName, data, err = export.FileName, export.HTML, exportErr
		}
		if err != nil {
			return sessionExportedMsg{err: err}
		}
		path := strings.TrimSpace(requestedPath)
		if path == "" {
			path = filepath.Join(cwd, fileName)
		} else if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		path, err = filepath.Abs(path)
		if err == nil {
			err = os.MkdirAll(filepath.Dir(path), 0o755)
		}
		if err == nil {
			err = os.WriteFile(path, data, 0o600)
		}
		return sessionExportedMsg{path: path, err: err}
	}
}

func renameSessionCmd(
	ctx context.Context,
	api application.API,
	sessionID, name string,
	sessionGeneration uint64,
) tea.Cmd {
	return func() tea.Msg {
		err := api.RenameSession(ctx, sessionID, name)
		return sessionMutationMsg{sessionGeneration: sessionGeneration, operation: "Rename", err: err}
	}
}

func deleteSessionCmd(
	ctx context.Context,
	api application.API,
	sessionID string,
	sessionGeneration uint64,
) tea.Cmd {
	return func() tea.Msg {
		err := api.DeleteSession(ctx, sessionID)
		return sessionMutationMsg{sessionGeneration: sessionGeneration, operation: "Delete", err: err}
	}
}

func projectTrustStatusCmd(ctx context.Context, api application.API, cwd string) tea.Cmd {
	return func() tea.Msg {
		status, err := api.ProjectTrust(ctx, cwd)
		return projectTrustMsg{status: status, err: err}
	}
}

func trustProjectCmd(ctx context.Context, api application.API, cwd string) tea.Cmd {
	return func() tea.Msg {
		status, err := api.TrustProject(ctx, cwd)
		return projectTrustMsg{status: status, applied: err == nil, err: err}
	}
}
