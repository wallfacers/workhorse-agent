## MODIFIED Requirements

### Requirement: System prompt injection

The system SHALL inject memory snapshot content into the system prompt of every
session via a single `{{.Memory}}` template variable rendered by the
`internal/prompt` package. The memory block SHALL be positioned **after** the
static base段, the `<environment>` block, and the `<instructions>` block
(组装顺序为 `base → environment → instructions → memory`，见 agent-loop spec
「System prompt 组装顺序优先静态前缀」), so that the static cache prefix precedes
the dynamic memory content. When both memory files are empty, the variable expands
to an empty string and the system prompt MUST NOT contain any memory-related
framing or headers.

#### Scenario: Non-empty memory rendered with stable delimiters

- **WHEN** a session has non-empty `MEMORY.md` or `USER.md` content
- **THEN** the rendered system prompt contains the memory text within byte-stable
  delimiters (so that prompt cache prefixes remain identical across turns of the
  same session), positioned after the base, environment, and instructions segments

#### Scenario: Empty memory produces no memory section

- **WHEN** both memory files are empty for a session
- **THEN** the rendered system prompt contains no memory-related text, headers, or
  delimiters
