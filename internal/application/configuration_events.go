package application

// ConfigurationResourceEvent invalidates scoped read caches after the owning
// service publishes configuration. Empty CWD denotes a global dependency.
// It does not reload an AgentSession or change its resource ownership.
type ConfigurationResourceEvent struct {
	CWD       string
	Models    bool
	Resources bool
}

func (ConfigurationResourceEvent) Type() EventType   { return EventConfigurationResource }
func (ConfigurationResourceEvent) applicationEvent() {}

func (s *Service) publishConfiguration(cwd string, models, resources bool) {
	s.events.publish(Event{Value: ConfigurationResourceEvent{CWD: cwd, Models: models, Resources: resources}})
}

func (s *Service) publishSkillConfiguration(cwd string, scope SkillInstallScope) {
	if scope == SkillScopeGlobal {
		cwd = ""
	}
	s.publishConfiguration(cwd, false, true)
}
