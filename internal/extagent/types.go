package extagent

import "time"

type AdapterClass string

const (
	ClassSubAgent AdapterClass = "sub_agent"
	ClassCLITool  AdapterClass = "cli_tool"
)

type Adapter struct {
	Name        string       `yaml:"name"`
	Binary      string       `yaml:"binary"`
	Class       AdapterClass `yaml:"class"`
	Invocation  Invocation   `yaml:"invocation"`
	Session     Session      `yaml:"session"`
	Output      Output       `yaml:"output"`
	Control     Control      `yaml:"control"`
	Security    Security     `yaml:"security"`
	SmokeTest   SmokeTest    `yaml:"smoke_test"`
	Description string       `yaml:"description"`
	UsageHints  string       `yaml:"usage_hints"`
	Provenance  Provenance   `yaml:"provenance"`

	// ResolvedBinary is the absolute path after LookPath / Stat.
	ResolvedBinary string
	// BinaryMissing is true when the binary cannot be resolved.
	BinaryMissing bool
	// SmokePassed is true after a successful smoke test (or cache hit).
	SmokePassed bool
	// SmokeError holds the error message from a failed smoke test.
	SmokeError string
	// LoadedAt records when this adapter was loaded into the registry.
	LoadedAt time.Time
}

type Invocation struct {
	PromptVia      string            `yaml:"prompt_via"`
	PromptArg      string            `yaml:"prompt_arg"`
	ExtraArgs      []string          `yaml:"extra_args"`
	Cwd            string            `yaml:"cwd"`
	EnvPassthrough []string          `yaml:"env_passthrough"`
	EnvOverride    map[string]string `yaml:"env_override"`
}

type Session struct {
	SupportsResume bool   `yaml:"supports_resume"`
	ResumeFlag     string `yaml:"resume_flag"`
	SessionIDArg   string `yaml:"session_id_arg"`
}

type Output struct {
	Format string  `yaml:"format"`
	Stderr string  `yaml:"stderr"`
	Parser *Parser `yaml:"parser"`
}

type Parser struct {
	AssistantText string `yaml:"assistant_text"`
	SessionIDPath string `yaml:"session_id_path"`
}

type Control struct {
	CancelSignal      string `yaml:"cancel_signal"`
	CancelGraceSec    int    `yaml:"cancel_grace_sec"`
	DefaultTimeoutSec int    `yaml:"default_timeout_sec"`
	MaxTimeoutSec     int    `yaml:"max_timeout_sec"`
}

type Security struct {
	Network    string `yaml:"network"`
	Filesystem string `yaml:"filesystem"`
	Trusted    bool   `yaml:"trusted"`
}

type SmokeTest struct {
	Prompt            string `yaml:"prompt"`
	ExpectedSubstring string `yaml:"expected_substring"`
	TimeoutSec        int    `yaml:"timeout_sec"`
}

type Provenance struct {
	Source      string `yaml:"source"`
	GeneratedBy string `yaml:"generated_by"`
	GeneratedAt string `yaml:"generated_at"`
	ToolVersion string `yaml:"tool_version"`
	ReviewedBy  string `yaml:"reviewed_by"`
}

// IsHealthy returns true if the adapter is usable: binary present and smoke passed.
func (a *Adapter) IsHealthy() bool {
	return !a.BinaryMissing && a.SmokePassed
}
