## 1. Config layer

- [x] 1.1 Add `DefaultPermission` and `PresetRules` fields to `ToolsConfig` in `internal/config/config.go`; add `PresetRule` type
- [x] 1.2 Add `Default()` values for new fields (`DefaultPermission: ""`, `PresetRules: nil`)
- [x] 1.3 Add validation in `internal/config/validate.go`: `default_permission` must be empty, `allow_permanent`, or `deny_permanent`; `preset_rules[].decision` must be `allow_permanent` or `deny_permanent`
- [x] 1.4 Add config validation tests for new fields (illegal values reject, empty/valid values pass)

## 2. Store layer

- [x] 2.1 Change `SavePermission` SQL from `INSERT INTO` to `INSERT OR REPLACE INTO` in `internal/store/sqlite/crud.go`
- [x] 2.2 Add/update store test to verify INSERT OR REPLACE behaviour (same ID overwrites)

## 3. Permission manager

- [x] 3.1 Add `defaultDecision` field to `Manager` struct in `internal/permission/manager.go`
- [x] 3.2 Update `New()` to accept `defaultDecision Decision` parameter
- [x] 3.3 Add step 4 fallback in `Check()`: if no rule matched and `m.defaultDecision != ""`, return it silently (skip prompt)
- [x] 3.4 Update `cmd/workhorse-agent/cmd_serve.go` to pass `defaultDecision` from config to `permission.New()`
- [x] 3.5 Add/update Manager tests: default_permission fallback returns expected decision; empty default still prompts; dangerous commands still prompt regardless

## 4. Preset rules injection

- [x] 4.1 Implement `applyPresetRules()` in `cmd/workhorse-agent/cmd_serve.go`: iterate `cfg.Tools.PresetRules`, compute deterministic ID via md5(tool+"\x00"+pattern+"\x00"+decision) hex[:16], call `SavePermission` with `INSERT OR REPLACE`
- [x] 4.2 Log "applied N preset permission rules" after injection
- [x] 4.3 Add `presetRuleID()` helper function
- [x] 4.4 Wire `applyPresetRules` call after store migration in server startup sequence

## 5. HTTP API

- [x] 5.1 Create `internal/api/permissions.go` with `handleListPermissions` (GET /v1/permissions), `handleCreatePermission` (POST /v1/permissions), `handleDeletePermission` (DELETE /v1/permissions/{id})
- [x] 5.2 Implement `source` field computation: compare rule ID against current preset rules' deterministic IDs
- [x] 5.3 Implement POST body validation: decision required, must be allow_permanent or deny_permanent; tool and pattern optional
- [x] 5.4 Add 3 routes to `internal/api/server.go` routes()
- [x] 5.5 Add permissions_test.go with handler tests: list empty, list with rules, create valid, create invalid decision, delete existing, delete not found, source detection (preset vs manual)

## 6. CLI

- [x] 6.1 Create `cmd/workhorse-agent/cmd_perm.go` with `permissions list`, `permissions add <tool> <pattern> <decision>`, `permissions remove <id>` subcommands
- [x] 6.2 CLI calls HTTP API on the running server (same pattern as sessions subcommand)
- [x] 6.3 Register permissions command in root command

## 7. Web UI

- [x] 7.1 Add permissions management panel to `web/index.html`: table (tool, pattern, decision, source, delete button), add form (tool dropdown, pattern input, decision select)
- [x] 7.2 Add permission CRUD API calls to `web/app.js`: fetch rules on load, send create/delete requests, update table on change
- [x] 7.3 Handle API errors in UI (show error message, retry hint)

## 8. Integration & edge cases

- [x] 8.1 Verify end-to-end: set `default_permission: allow_permanent` + `preset_rules`, start server, confirm rules appear in GET /v1/permissions, confirm tool calls skip prompt
- [x] 8.2 Verify dangerous commands still prompt regardless of default_permission or preset rules
- [x] 8.3 Verify deny_permanent preset rule takes precedence over default_permission: allow_permanent
- [x] 8.4 Verify preset rules are recreated on restart after API deletion
