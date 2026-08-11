package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/application"
)

type worktreeHandlerAPI struct {
	application.API
	listResult application.WorktreeList
	listErr    error
	removeErr  error
}

func (api worktreeHandlerAPI) ListWorktrees(context.Context, string) (application.WorktreeList, error) {
	return api.listResult, api.listErr
}

func (api worktreeHandlerAPI) RemoveWorktree(context.Context, string, string, bool) error {
	return api.removeErr
}

func TestWorktreeHTTPContract(t *testing.T) {
	branch := "main"
	api := worktreeHandlerAPI{listResult: application.WorktreeList{
		ProjectRoot: "/project", IsGit: true, IsTopLevel: true,
		Worktrees: []application.WorktreeInfo{{Path: "/project", Branch: &branch, IsMain: true}},
	}}
	listRequest := httptest.NewRequest(http.MethodGet, "/api/v1/worktrees?cwd=%2Fproject", nil)
	listResponse := httptest.NewRecorder()
	handleWorktreeList(api).ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK || !strings.Contains(listResponse.Body.String(), `"projectRoot":"/project"`) || !strings.Contains(listResponse.Body.String(), `"isMain":true`) {
		t.Fatalf("list response = %d %s", listResponse.Code, listResponse.Body.String())
	}

	dirtyAPI := worktreeHandlerAPI{removeErr: errors.New("worktree contains modified or untracked files")}
	removeRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/worktrees", bytes.NewBufferString(`{"cwd":"/project","path":"/worktree"}`))
	removeRequest.Header.Set("Content-Type", "application/json")
	removeResponse := httptest.NewRecorder()
	handleWorktreeRemove(dirtyAPI).ServeHTTP(removeResponse, removeRequest)
	if removeResponse.Code != http.StatusConflict || !strings.Contains(removeResponse.Body.String(), `"dirty":true`) {
		t.Fatalf("dirty response = %d %s", removeResponse.Code, removeResponse.Body.String())
	}
}

func TestWorktreeHTTPRejectsDeniedRoot(t *testing.T) {
	api := worktreeHandlerAPI{listErr: application.ErrResourceAccessDenied}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/worktrees?cwd=%2Fprivate", nil)
	response := httptest.NewRecorder()
	handleWorktreeList(api).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "Access denied") {
		t.Fatalf("denied response = %d %s", response.Code, response.Body.String())
	}
}
