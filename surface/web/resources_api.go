package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/cat3399/pi-go/internal/application"
)

func handleProjectTrust(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		cwd, err := normalizeUserCWD(request.URL.Query().Get("cwd"))
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, err)
			return
		}
		status, err := api.ProjectTrust(request.Context(), cwd)
		if err != nil {
			writeAPIError(writer, http.StatusInternalServerError, err)
			return
		}
		writeProjectTrust(writer, status)
	}
}

func handleTrustProject(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, err := readRequestBody(writer, request)
		if err != nil {
			writeRequestBodyError(writer, err)
			return
		}
		var input struct {
			CWD string `json:"cwd"`
		}
		if json.Unmarshal(body, &input) != nil {
			writeAPIError(writer, http.StatusBadRequest, errors.New("cwd must be a string"))
			return
		}
		cwd, err := normalizeUserCWD(input.CWD)
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, err)
			return
		}
		status, err := api.TrustProject(request.Context(), cwd)
		if err != nil {
			if errors.Is(err, application.ErrProjectTrustNotRequired) || errors.Is(err, application.ErrProjectBusy) {
				writeAPIError(writer, http.StatusConflict, err)
			} else {
				writeAPIError(writer, http.StatusInternalServerError, err)
			}
			return
		}
		writeProjectTrust(writer, status)
	}
}

func writeProjectTrust(writer http.ResponseWriter, status application.ProjectTrustStatus) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"requiresTrust": status.RequiresTrust,
		"trusted":       status.Trusted,
	})
}

func handleSkills(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		cwd, err := normalizeUserCWD(request.URL.Query().Get("cwd"))
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, err)
			return
		}
		snapshot, err := api.ListSkills(request.Context(), cwd)
		if err != nil {
			writeAPIError(writer, http.StatusInternalServerError, err)
			return
		}
		skills := make([]any, len(snapshot.Skills))
		for index, skill := range snapshot.Skills {
			skills[index] = skillWire(skill)
		}
		diagnostics := make([]any, len(snapshot.Diagnostics))
		for index, diagnostic := range snapshot.Diagnostics {
			diagnostics[index] = resourceDiagnosticWire(diagnostic)
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"skills": skills, "diagnostics": diagnostics,
			"projectResourcesLoaded": snapshot.ProjectResourcesLoaded,
		})
	}
}

func handleSkillToggle(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, err := readRequestBody(writer, request)
		if err != nil {
			writeRequestBodyError(writer, err)
			return
		}
		var input struct {
			CWD                    string `json:"cwd"`
			FilePath               string `json:"filePath"`
			DisableModelInvocation *bool  `json:"disableModelInvocation"`
		}
		if json.Unmarshal(body, &input) != nil || strings.TrimSpace(input.CWD) == "" || strings.TrimSpace(input.FilePath) == "" || input.DisableModelInvocation == nil {
			writeAPIError(writer, http.StatusBadRequest, errors.New("cwd, filePath, and disableModelInvocation are required"))
			return
		}
		cwd, err := normalizeUserCWD(input.CWD)
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, err)
			return
		}
		if err := api.SetSkillModelInvocation(request.Context(), cwd, input.FilePath, *input.DisableModelInvocation); err != nil {
			switch {
			case errors.Is(err, os.ErrPermission):
				writeAPIError(writer, http.StatusForbidden, errors.New("access denied"))
			case errors.Is(err, os.ErrNotExist):
				writeAPIError(writer, http.StatusNotFound, errors.New("skill file not found"))
			default:
				writeAPIError(writer, http.StatusInternalServerError, err)
			}
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"success": true})
	}
}

func handleSkillSearch(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, err := readRequestBody(writer, request)
		if err != nil {
			writeRequestBodyError(writer, err)
			return
		}
		var input struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if json.Unmarshal(body, &input) != nil || strings.TrimSpace(input.Query) == "" {
			writeAPIError(writer, http.StatusBadRequest, errors.New("query is required"))
			return
		}
		results, err := api.SearchSkills(request.Context(), input.Query, input.Limit)
		if err != nil {
			writeAPIError(writer, http.StatusBadGateway, err)
			return
		}
		encoded := make([]any, len(results))
		for index, result := range results {
			encoded[index] = map[string]any{"package": result.Package, "installs": result.Installs, "url": result.URL}
		}
		writeJSON(writer, http.StatusOK, map[string]any{"results": encoded})
	}
}

func handleSkillInstall(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, err := readRequestBody(writer, request)
		if err != nil {
			writeRequestBodyError(writer, err)
			return
		}
		var input struct {
			Package string                        `json:"package"`
			Scope   application.SkillInstallScope `json:"scope"`
			CWD     string                        `json:"cwd"`
		}
		if json.Unmarshal(body, &input) != nil || strings.TrimSpace(input.Package) == "" {
			writeAPIError(writer, http.StatusBadRequest, errors.New("package is required"))
			return
		}
		if input.Scope == application.SkillScopeProject && strings.TrimSpace(input.CWD) == "" {
			writeAPIError(writer, http.StatusBadRequest, errors.New("cwd is required for project install"))
			return
		}
		if input.CWD != "" {
			input.CWD, err = normalizeUserCWD(input.CWD)
			if err != nil {
				writeAPIError(writer, http.StatusBadRequest, err)
				return
			}
		}
		skill, err := api.InstallSkill(request.Context(), application.SkillInstallRequest{
			Package: input.Package, Scope: input.Scope, CWD: input.CWD,
		})
		if err != nil {
			writeSkillMutationError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"success": true, "skill": skillWire(skill)})
	}
}

func handleSkillCheck(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		input, ok := readSkillUpdateRequest(writer, request)
		if !ok {
			return
		}
		updates, err := api.CheckSkillUpdates(request.Context(), input)
		if err != nil {
			if errors.Is(err, application.ErrInstalledSkillMissing) {
				writeAPIError(writer, http.StatusNotFound, err)
			} else {
				writeAPIError(writer, http.StatusInternalServerError, err)
			}
			return
		}
		encoded := make([]any, len(updates))
		for index, update := range updates {
			encoded[index] = skillUpdateWire(update)
		}
		writeJSON(writer, http.StatusOK, map[string]any{"updates": encoded})
	}
}

func handleSkillUpdate(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		input, ok := readSkillUpdateRequest(writer, request)
		if !ok {
			return
		}
		if input.Package == "" || input.Scope == "" {
			writeAPIError(writer, http.StatusBadRequest, errors.New("cwd, package, and scope are required"))
			return
		}
		skill, err := api.UpdateSkill(request.Context(), input)
		if err != nil {
			writeSkillMutationError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"success": true, "skill": skillWire(skill)})
	}
}

func readSkillUpdateRequest(writer http.ResponseWriter, request *http.Request) (application.SkillUpdateRequest, bool) {
	body, err := readRequestBody(writer, request)
	if err != nil {
		writeRequestBodyError(writer, err)
		return application.SkillUpdateRequest{}, false
	}
	var input struct {
		CWD     string                        `json:"cwd"`
		Package string                        `json:"package"`
		Scope   application.SkillInstallScope `json:"scope"`
	}
	if json.Unmarshal(body, &input) != nil || strings.TrimSpace(input.CWD) == "" {
		writeAPIError(writer, http.StatusBadRequest, errors.New("cwd is required"))
		return application.SkillUpdateRequest{}, false
	}
	cwd, err := normalizeUserCWD(input.CWD)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, err)
		return application.SkillUpdateRequest{}, false
	}
	if (input.Package == "") != (input.Scope == "") {
		writeAPIError(writer, http.StatusBadRequest, errors.New("package and scope must be provided together"))
		return application.SkillUpdateRequest{}, false
	}
	if input.Scope != "" && input.Scope != application.SkillScopeGlobal && input.Scope != application.SkillScopeProject {
		writeAPIError(writer, http.StatusBadRequest, errors.New("scope must be global or project"))
		return application.SkillUpdateRequest{}, false
	}
	return application.SkillUpdateRequest{CWD: cwd, Package: input.Package, Scope: input.Scope}, true
}

func writeSkillMutationError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrPermission):
		writeAPIError(writer, http.StatusForbidden, errors.New("project resources must be trusted before installing project skills"))
	case errors.Is(err, application.ErrSkillAlreadyInstalled):
		writeAPIError(writer, http.StatusConflict, err)
	case errors.Is(err, application.ErrInstalledSkillMissing):
		writeAPIError(writer, http.StatusNotFound, err)
	case errors.Is(err, application.ErrSkillUpdateUnsupported):
		writeAPIError(writer, http.StatusBadRequest, err)
	case errors.Is(err, application.ErrInvalidSkillRequest):
		writeAPIError(writer, http.StatusBadRequest, err)
	default:
		writeAPIError(writer, http.StatusInternalServerError, err)
	}
}

func skillWire(skill application.SkillInfo) map[string]any {
	value := map[string]any{
		"name": skill.Name, "description": skill.Description, "filePath": skill.FilePath,
		"baseDir": skill.BaseDir, "disableModelInvocation": skill.DisableModelInvocation,
		"sourceInfo": map[string]any{"source": skill.SourceInfo.Source, "scope": skill.SourceInfo.Scope},
	}
	if skill.Install != nil {
		install := map[string]any{
			"package": skill.Install.Package, "scope": skill.Install.Scope,
			"source": skill.Install.Source, "canCheckForUpdates": skill.Install.CanCheckForUpdates,
		}
		for key, field := range map[string]string{
			"sourceType": skill.Install.SourceType, "skillsShUrl": skill.Install.SkillsShURL,
			"skillPath": skill.Install.SkillPath, "ref": skill.Install.Ref, "versionHash": skill.Install.VersionHash,
		} {
			if field != "" {
				install[key] = field
			}
		}
		value["install"] = install
	}
	return value
}

func resourceDiagnosticWire(diagnostic application.ResourceDiagnostic) map[string]any {
	value := map[string]any{"type": diagnostic.Type, "message": diagnostic.Message}
	if diagnostic.Path != "" {
		value["path"] = diagnostic.Path
	}
	if diagnostic.Collision != nil {
		value["collision"] = map[string]any{
			"resourceType": diagnostic.Collision.ResourceType, "name": diagnostic.Collision.Name,
			"winnerPath": diagnostic.Collision.WinnerPath, "loserPath": diagnostic.Collision.LoserPath,
			"winnerSource": diagnostic.Collision.WinnerSource, "loserSource": diagnostic.Collision.LoserSource,
		}
	}
	return value
}

func skillUpdateWire(update application.SkillUpdateResult) map[string]any {
	value := map[string]any{"package": update.Package, "scope": update.Scope, "state": update.State}
	if update.CurrentVersion != "" {
		value["currentVersion"] = update.CurrentVersion
	}
	if update.LatestVersion != "" {
		value["latestVersion"] = update.LatestVersion
	}
	if update.Message != "" {
		value["message"] = update.Message
	}
	return value
}
