package config

import "reflect"

// ReloadableDiff reports what changed between two configs from the hot-reload
// perspective: which permission-subset fields changed (and may be applied at
// runtime) and which non-reloadable fields changed (and only take effect on
// restart).
type ReloadableDiff struct {
	PresetRulesChanged       bool
	DefaultPermissionChanged bool
	TimeoutChanged           bool
	// NonReloadable lists human-readable names of fields that changed but
	// cannot be applied without a restart (e.g. "server.port", "store.path").
	NonReloadable []string
}

// HasReloadable reports whether any runtime-applicable field changed.
func (d ReloadableDiff) HasReloadable() bool {
	return d.PresetRulesChanged || d.DefaultPermissionChanged || d.TimeoutChanged
}

// DiffReloadable classifies the differences between the currently-applied
// config and a freshly-loaded one. The hot-reload path applies the reloadable
// fields and warns about the rest.
func DiffReloadable(oldCfg, newCfg Config) ReloadableDiff {
	var d ReloadableDiff
	d.PresetRulesChanged = !reflect.DeepEqual(oldCfg.Tools.PresetRules, newCfg.Tools.PresetRules)
	d.DefaultPermissionChanged = oldCfg.Tools.DefaultPermission != newCfg.Tools.DefaultPermission
	d.TimeoutChanged = oldCfg.Agent.PermissionRequestTimeoutSeconds != newCfg.Agent.PermissionRequestTimeoutSeconds

	// Normalise the reloadable fields to equal, then anything still different is
	// non-reloadable.
	a, b := oldCfg, newCfg
	b.Tools.PresetRules = a.Tools.PresetRules
	b.Tools.DefaultPermission = a.Tools.DefaultPermission
	b.Agent.PermissionRequestTimeoutSeconds = a.Agent.PermissionRequestTimeoutSeconds
	if !reflect.DeepEqual(a, b) {
		d.NonReloadable = nonReloadableFields(a, b)
	}
	return d
}

// nonReloadableFields names the changed non-reloadable fields. It checks a
// curated set for friendly messages and falls back to a generic marker so an
// unenumerated change is never silently swallowed.
func nonReloadableFields(a, b Config) []string {
	var names []string
	add := func(name string, changed bool) {
		if changed {
			names = append(names, name)
		}
	}
	add("server.host", a.Server.Host != b.Server.Host)
	add("server.port", a.Server.Port != b.Server.Port)
	add("server.default_workdir", a.Server.DefaultWorkdir != b.Server.DefaultWorkdir)
	add("store.path", a.Store.Path != b.Store.Path)
	add("auth.enabled", a.Auth.Enabled != b.Auth.Enabled)
	add("auth.bearer_token", a.Auth.BearerToken != b.Auth.BearerToken)
	add("providers.default", a.Providers.Default != b.Providers.Default)
	add("providers", !reflect.DeepEqual(a.Providers, b.Providers) && a.Providers.Default == b.Providers.Default)
	add("models", !reflect.DeepEqual(a.Models, b.Models))
	add("logging.level", a.Logging.Level != b.Logging.Level)
	add("sessions.max_concurrent", a.Sessions.MaxConcurrent != b.Sessions.MaxConcurrent)

	if len(names) == 0 {
		// Something outside the curated set changed; surface it generically.
		names = append(names, "other")
	}
	return names
}
