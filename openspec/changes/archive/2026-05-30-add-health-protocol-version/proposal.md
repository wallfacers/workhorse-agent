# Proposal: Record protocol_version and capabilities on GET /health in the spec

## Why

The desktop frontend (workhorse-assistant) is moving to **auto-connect**: on
startup it probes `GET /health` and only attaches a session if the listener is a
compatible workhorse-agent. To verify compatibility before attaching it reads two
fields:

- `protocol_version` — the wire protocol the server speaks. The frontend refuses
  to attach (and stops retrying) on a mismatch, so it can tell "the sidecar is
  not yet up" (retry) apart from "this is an incompatible build" (don't retry).
- `capabilities` — the named features the server supports; the frontend gates on
  `frontend_tools`.

The **code already returns both fields** (`internal/api/health.go` defines
`ProtocolVersion = "1"` and `DefaultCapabilities` and emits them from
`handleHealth`). What is missing is twofold:

1. The consolidated `api-protocol` spec's "Health and Diagnostics" requirement
   does not mention `protocol_version`/`capabilities` — the archived
   `add-frontend-tool-bridge` change specified the scenario but it was never
   merged into the live spec. The spec is the source of truth, so this drift must
   be closed (`spec-as-contract`).
2. `health_test.go` asserts only `ok`/`version`/`uptime_sec`/`sessions_active`,
   leaving the two fields the frontend depends on uncovered.

This change reconciles the spec and the test suite with the already-shipped code
and the frontend's auto-connect contract. No behavioural code change is needed.

## What Changes

- Update the `api-protocol` "Health and Diagnostics Endpoints" requirement so it
  states `GET /health` returns `protocol_version` and `capabilities` (additive,
  backward compatible), and remains auth/Origin-exempt.
- Extend `health_test.go` to assert `protocol_version == "1"` and that
  `capabilities` contains `frontend_tools`.
- No production code change: `ProtocolVersion`/`DefaultCapabilities` and the
  handler already exist in `internal/api/health.go`.

## Capabilities

### Modified Capabilities

- `api-protocol`: the Health and Diagnostics Endpoints requirement is updated so
  `GET /health` additionally documents `protocol_version` and `capabilities`.

## Impact

- **Code**: test only — `internal/api/health_test.go`. The handler and constants
  in `internal/api/health.go` are unchanged (already implemented).
- **Cross-repo**: confirms the contract workhorse-assistant's `add-auto-connect`
  change relies on (Rust `EXPECTED_PROTOCOL_VERSION` ↔ Go `ProtocolVersion`,
  both `"1"`); `feedback_multi-agent-git-coordination`.
- **Backward compatibility**: spec/test only; the wire response is unchanged.
- **Out of scope**: moving the constants into another package; capability
  negotiation; bumping the protocol version; new endpoints.
