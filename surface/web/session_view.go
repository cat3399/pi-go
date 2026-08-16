package web

import (
	"encoding/json"

	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/session"
	"github.com/cat3399/pi-go/internal/surfacewire"
)

// Web keeps these aliases while HTTP and GUI IPC share one canonical
// presentation projection in internal/surfacewire.
type selectedModelWire = surfacewire.SelectedModel
type sessionInfoWire = surfacewire.SessionInfo
type sessionContextWire = surfacewire.SessionContext
type treeNodeWire = surfacewire.TreeNode
type sessionViewWire = surfacewire.SessionView

const maxProjectedTreeDepth = surfacewire.MaxProjectedTreeDepth

func normalizeHistoryToolCalls(message json.RawMessage) (json.RawMessage, error) {
	return surfacewire.NormalizeHistoryToolCalls(message)
}

func deferHistoryMedia(message json.RawMessage, deferThinking, deferMedia bool) (json.RawMessage, error) {
	return surfacewire.DeferHistoryMedia(message, deferThinking, deferMedia)
}

func listSessions(api application.API) ([]sessionInfoWire, error) {
	return surfacewire.ListSessions(api)
}

func sessionInfoToWire(info application.SessionInfo) sessionInfoWire {
	return surfacewire.SessionInfoFromApplication(info)
}

func sessionView(api application.API, id, leafID string, deferThinking, deferMedia bool) (sessionViewWire, error) {
	return surfacewire.SessionViewFor(api, id, leafID, deferThinking, deferMedia)
}

func sessionContext(api application.API, id, leafID string, deferThinking, deferMedia bool) (sessionContextWire, error) {
	return surfacewire.SessionContextFor(api, id, leafID, deferThinking, deferMedia)
}

func buildSessionContext(snapshot application.SessionSnapshot, deferThinking, deferMedia bool) (sessionContextWire, error) {
	return surfacewire.BuildSessionContext(snapshot, deferThinking, deferMedia)
}

func entryToUIMessage(entry session.Entry, deferThinking, deferMedia bool) (json.RawMessage, bool, error) {
	return surfacewire.ProjectEntry(entry, deferThinking, deferMedia)
}

func projectSessionTree(forest []session.TreeNode) ([]*treeNodeWire, error) {
	return surfacewire.ProjectSessionTree(forest)
}

func stateModel(state application.State) *selectedModelWire {
	return surfacewire.StateModel(state)
}
