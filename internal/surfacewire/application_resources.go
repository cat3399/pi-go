package surfacewire

import (
	"encoding/base64"
	"encoding/json"

	"github.com/cat3399/pi-go/internal/application"
)

type ApplicationReadRequest struct {
	Project string `json:"project,omitempty"`
	Search  string `json:"search,omitempty"`
	Cursor  string `json:"cursor,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

type Overview struct {
	AgentDir          string        `json:"agentDir"`
	DefaultCWD        string        `json:"defaultCwd"`
	Projects          []ProjectInfo `json:"projects"`
	SessionCount      int           `json:"sessionCount"`
	RunningSessionIDs []string      `json:"runningSessionIds"`
}

type SessionDirectoryPage struct {
	Version  string        `json:"version"`
	Sessions []SessionInfo `json:"sessions"`
	Total    int           `json:"total"`
	Start    int           `json:"start"`
	Next     string        `json:"next,omitempty"`
}

func ReadApplicationResource(api application.API, kind string, query ApplicationReadRequest, known string) (ResourceResponse, error) {
	switch kind {
	case "overview":
		value, err := api.ReadOverview()
		if err != nil {
			return ResourceResponse{}, err
		}
		result, err := resourceResponse(Overview{AgentDir: value.AgentDir, DefaultCWD: value.DefaultCWD,
			Projects: projectsFromApplication(value.Projects), SessionCount: value.SessionCount, RunningSessionIDs: value.RunningSessionIDs}, known)
		// This is a synchronization anchor, independent of the validated data.
		result.Cursor = EncodeEventCursor(value.Cursor)
		return result, err
	case "directory":
		var cursor *application.SessionDirectoryCursor
		if query.Cursor != "" {
			if len(query.Cursor) > 2048 {
				return ResourceResponse{}, ErrInvalidRequest
			}
			data, err := base64.RawURLEncoding.DecodeString(query.Cursor)
			if err != nil || json.Unmarshal(data, &cursor) != nil || cursor == nil {
				return ResourceResponse{}, ErrInvalidRequest
			}
		}
		value, err := api.ReadSessionDirectory(application.SessionDirectoryQuery{Project: query.Project, Search: query.Search, Cursor: cursor, Limit: query.Limit})
		if err != nil {
			return ResourceResponse{}, err
		}
		page := SessionDirectoryPage{Version: value.Version, Total: value.Total, Start: value.Start, Sessions: make([]SessionInfo, 0, len(value.Sessions))}
		for _, info := range value.Sessions {
			page.Sessions = append(page.Sessions, SessionInfoFromApplication(info))
		}
		if value.Next != nil {
			data, _ := json.Marshal(value.Next)
			page.Next = base64.RawURLEncoding.EncodeToString(data)
		}
		return resourceResponse(page, known)
	default:
		return ResourceResponse{}, ErrInvalidRequest
	}
}
