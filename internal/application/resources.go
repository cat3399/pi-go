package application

import (
	"context"
	"errors"
	"fmt"

	"github.com/cat3399/pi-go/internal/installation"
	modelcatalog "github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/resource"
)

var (
	ErrProjectTrustNotRequired = errors.New("project has no resources that require trust")
	ErrProjectBusy             = errors.New("project has an active busy session")
)

type ProjectTrustStatus struct {
	RequiresTrust bool
	Trusted       bool
}

func (s *Service) ProjectTrust(ctx context.Context, cwd string) (ProjectTrustStatus, error) {
	cwd, err := ValidateCWD(cwd)
	if err != nil {
		return ProjectTrustStatus{}, err
	}
	if err := installation.InitializeProject(normalizeContext(ctx), cwd); err != nil {
		return ProjectTrustStatus{}, err
	}
	requires := resource.HasTrustRequiringProjectResources(cwd)
	if !requires {
		return ProjectTrustStatus{Trusted: true}, nil
	}
	store, err := resource.NewTrustStore(s.paths.AgentDir)
	if err != nil {
		return ProjectTrustStatus{}, err
	}
	trusted, _, err := store.Get(normalizeContext(ctx), cwd)
	if err != nil {
		return ProjectTrustStatus{}, err
	}
	return ProjectTrustStatus{RequiresTrust: true, Trusted: trusted}, nil
}

func (s *Service) TrustProject(ctx context.Context, cwd string) (ProjectTrustStatus, error) {
	ctx = normalizeContext(ctx)
	cwd, err := ValidateCWD(cwd)
	if err != nil {
		return ProjectTrustStatus{}, err
	}
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	status, err := s.ProjectTrust(ctx, cwd)
	if err != nil {
		return ProjectTrustStatus{}, err
	}
	if !status.RequiresTrust {
		return ProjectTrustStatus{}, ErrProjectTrustNotRequired
	}

	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	for _, managed := range s.activeSessions() {
		_, managedCWD, _ := managed.identity()
		if cleanPathKey(managedCWD) == cleanPathKey(cwd) && managed.busy() {
			return ProjectTrustStatus{}, ErrProjectBusy
		}
	}
	store, err := resource.NewTrustStore(s.paths.AgentDir)
	if err != nil {
		return ProjectTrustStatus{}, err
	}
	if err := store.Set(ctx, cwd, true); err != nil {
		return ProjectTrustStatus{}, err
	}

	ids := make(map[string]struct{})
	for _, managed := range s.activeSessions() {
		id, managedCWD, _ := managed.identity()
		if cleanPathKey(managedCWD) == cleanPathKey(cwd) {
			ids[id] = struct{}{}
		}
	}
	detached := s.detachSessions(ids)
	var disposeErr error
	for _, managed := range detached {
		id, _, _ := managed.identity()
		disposeErr = errors.Join(disposeErr, managed.dispose(ctx))
		s.events.publish(Event{SessionID: id, Value: SessionCatalogEvent{Change: SessionUpdated}})
	}
	if disposeErr != nil {
		return ProjectTrustStatus{}, fmt.Errorf("restart sessions after trusting project: %w", disposeErr)
	}
	return ProjectTrustStatus{RequiresTrust: true, Trusted: true}, nil
}

func (s *Service) loadResourceSnapshot(ctx context.Context, cwd string) (resource.Snapshot, error) {
	cwd, err := ValidateCWD(cwd)
	if err != nil {
		return resource.Snapshot{}, err
	}
	if err := installation.InitializeProject(normalizeContext(ctx), cwd); err != nil {
		return resource.Snapshot{}, err
	}
	bootstrap, err := resource.New(resource.Config{CWD: cwd, AgentDir: s.paths.AgentDir})
	if err != nil {
		return resource.Snapshot{}, err
	}
	if err := bootstrap.Reload(normalizeContext(ctx)); err != nil {
		return resource.Snapshot{}, err
	}
	admission, err := bootstrap.Snapshot()
	if err != nil {
		return resource.Snapshot{}, err
	}
	settings, err := modelcatalog.LoadEffectiveSettings(s.paths.AgentDir, cwd, admission.Trusted)
	if err != nil {
		return resource.Snapshot{}, fmt.Errorf("load resource settings: %w", err)
	}
	service, err := resource.New(resource.Config{
		CWD: cwd, AgentDir: s.paths.AgentDir,
		SkillPaths: append([]string(nil), settings.Skills...), PromptPaths: append([]string(nil), settings.Prompts...),
	})
	if err != nil {
		return resource.Snapshot{}, err
	}
	if err := service.Reload(normalizeContext(ctx)); err != nil {
		return resource.Snapshot{}, err
	}
	return service.Snapshot()
}
