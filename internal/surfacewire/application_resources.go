package surfacewire

import (
	"encoding/base64"
	"encoding/json"

	"github.com/cat3399/pi-go/internal/application"
)

type ApplicationReadRequest struct {
	Project      string `json:"project,omitempty"`
	Search       string `json:"search,omitempty"`
	Cursor       string `json:"cursor,omitempty"`
	Limit        int    `json:"limit,omitempty"`
	SessionLimit int    `json:"sessionLimit,omitempty"`
}

type Overview struct {
	AgentDir          string               `json:"agentDir"`
	DefaultCWD        string               `json:"defaultCwd"`
	Projects          []ProjectInfo        `json:"projects"`
	SessionCount      int                  `json:"sessionCount"`
	RunningSessionIDs []string             `json:"runningSessionIds"`
	Directory         ProjectDirectoryPage `json:"directory"`
}

type ProjectDirectoryItem struct {
	Project  ProjectInfo          `json:"project"`
	Sessions SessionDirectoryPage `json:"sessions"`
}

type ProjectDirectoryPage struct {
	Version  string                 `json:"version"`
	Projects []ProjectDirectoryItem `json:"projects"`
	Start    int                    `json:"start"`
	Total    int                    `json:"total"`
	Next     string                 `json:"next,omitempty"`
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
			Projects: projectsFromApplication(value.Projects), SessionCount: value.SessionCount, RunningSessionIDs: value.RunningSessionIDs,
			Directory: projectDirectoryPage(value.Directory)}, known)
		// This is a synchronization anchor, independent of the validated data.
		result.Cursor = EncodeEventCursor(value.Cursor)
		return result, err
	case "projects":
		var cursor *application.ProjectDirectoryCursor
		if query.Cursor != "" {
			if len(query.Cursor) > 2048 {
				return ResourceResponse{}, ErrInvalidRequest
			}
			data, err := base64.RawURLEncoding.DecodeString(query.Cursor)
			if err != nil || json.Unmarshal(data, &cursor) != nil || cursor == nil {
				return ResourceResponse{}, ErrInvalidRequest
			}
		}
		value, err := api.ReadProjectDirectory(application.ProjectDirectoryQuery{Cursor: cursor, Limit: query.Limit, SessionLimit: query.SessionLimit})
		if err != nil {
			return ResourceResponse{}, err
		}
		return resourceResponse(projectDirectoryPage(value), known)
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
		return resourceResponse(sessionDirectoryPage(value), known)
	default:
		return ResourceResponse{}, ErrInvalidRequest
	}
}

func sessionDirectoryPage(value application.SessionDirectoryPage) SessionDirectoryPage {
	page := SessionDirectoryPage{Version: value.Version, Total: value.Total, Start: value.Start, Sessions: make([]SessionInfo, 0, len(value.Sessions))}
	for _, info := range value.Sessions {
		page.Sessions = append(page.Sessions, SessionInfoFromApplication(info))
	}
	if value.Next != nil {
		data, _ := json.Marshal(value.Next)
		page.Next = base64.RawURLEncoding.EncodeToString(data)
	}
	return page
}

func projectDirectoryPage(value application.ProjectDirectoryPage) ProjectDirectoryPage {
	page := ProjectDirectoryPage{Version: value.Version, Start: value.Start, Total: value.Total, Projects: []ProjectDirectoryItem{}}
	for _, item := range value.Projects {
		project := projectsFromApplication([]application.ProjectInfo{item.Project})[0]
		page.Projects = append(page.Projects, ProjectDirectoryItem{Project: project, Sessions: sessionDirectoryPage(item.Sessions)})
	}
	if value.Next != nil {
		data, _ := json.Marshal(value.Next)
		page.Next = base64.RawURLEncoding.EncodeToString(data)
	}
	return page
}
