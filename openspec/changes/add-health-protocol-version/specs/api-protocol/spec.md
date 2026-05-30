## MODIFIED Requirements

### Requirement: Health and Diagnostics Endpoints

The server SHALL expose operational endpoints for liveness and debugging. The
`GET /health` response SHALL include a `protocol_version` string identifying the
wire protocol the server speaks and a `capabilities` string array listing the
named features it supports, in addition to the existing `ok`, `version`,
`uptime_sec`, and `sessions_active` fields. These additions are backward
compatible: consumers that read only the pre-existing fields are unaffected. The
`GET /health` endpoint remains exempt from bearer-token auth and Origin checks.

#### Scenario: Health check

- **WHEN** a client sends `GET /health`
- **THEN** the server responds `200 OK` with JSON containing `ok`, `version`, `uptime_sec`, and `sessions_active`

#### Scenario: Protocol version and capabilities exposed

- **WHEN** a client sends `GET /health`
- **THEN** the response additionally includes `protocol_version` (e.g. `"1"`) and `capabilities` (a string array)
- **AND** `capabilities` contains `frontend_tools`

#### Scenario: Health probe before authenticating

- **WHEN** a client sends `GET /health` without a bearer token or with a foreign `Origin`
- **THEN** the server still responds `200 OK` (the endpoint is auth- and Origin-exempt)

#### Scenario: Session inspection

- **WHEN** a client sends `GET /debug/sessions`
- **THEN** the server responds with a JSON list of active session summaries
