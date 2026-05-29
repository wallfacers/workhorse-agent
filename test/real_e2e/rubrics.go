//go:build real_e2e

package real_e2e

import "github.com/wallfacers/workhorse-agent/test/real_e2e/judge"

var fileToolsRubric = judge.Rubric{
	MinScore:   0.7,
	MaxRetries: 2,
	Criteria: []judge.Criterion{
		{
			Name:        "tool_call_correct",
			Description: "Did the model call the correct tool with correct parameters (e.g., Read with the right file path)?",
			Weight:      0.3,
			Required:    true,
		},
		{
			Name:        "response_accuracy",
			Description: "Does the response accurately reflect the tool result content without fabrication?",
			Weight:      0.35,
			Required:    true,
		},
		{
			Name:        "no_hallucination",
			Description: "Did the model avoid fabricating information not present in the tool result?",
			Weight:      0.2,
			Required:    true,
		},
		{
			Name:        "efficiency",
			Description: "Did the model avoid unnecessary extra tool calls?",
			Weight:      0.15,
		},
	},
}

var fileNotFoundRubric = judge.Rubric{
	MinScore:   0.7,
	MaxRetries: 2,
	Criteria: []judge.Criterion{
		{
			Name:        "error_detected",
			Description: "Did the model correctly call Read on the nonexistent path?",
			Weight:      0.3,
			Required:    true,
		},
		{
			Name:        "error_reported",
			Description: "Did the model report the file-not-found error to the user?",
			Weight:      0.4,
			Required:    true,
		},
		{
			Name:        "no_fabrication",
			Description: "Did the model avoid making up file contents that don't exist?",
			Weight:      0.3,
			Required:    true,
		},
	},
}

var memoryRubric = judge.Rubric{
	MinScore:   0.8,
	MaxRetries: 1,
	Criteria: []judge.Criterion{
		{
			Name:        "tool_invocation",
			Description: "Did the model use memory_read/memory_write correctly with the right parameters?",
			Weight:      0.3,
			Required:    true,
		},
		{
			Name:        "data_integrity",
			Description: "Is the data read back identical to what was written?",
			Weight:      0.4,
			Required:    true,
		},
		{
			Name:        "cross_session",
			Description: "Can a new session see the memory written by a previous session?",
			Weight:      0.3,
			Required:    true,
		},
	},
}

var sessionSearchRubric = judge.Rubric{
	MinScore:   0.7,
	MaxRetries: 2,
	Criteria: []judge.Criterion{
		{
			Name:        "search_called",
			Description: "Did the model call session_search with an appropriate query?",
			Weight:      0.3,
			Required:    true,
		},
		{
			Name:        "results_accurate",
			Description: "Did the model accurately report the search results?",
			Weight:      0.4,
			Required:    true,
		},
		{
			Name:        "relevance",
			Description: "Did the model correctly identify which results are relevant to the query?",
			Weight:      0.3,
		},
	},
}

var extAgentRubric = judge.Rubric{
	MinScore:   0.7,
	MaxRetries: 2,
	Criteria: []judge.Criterion{
		{
			Name:        "correct_invocation",
			Description: "Did the model invoke the right external agent with appropriate arguments?",
			Weight:      0.4,
			Required:    true,
		},
		{
			Name:        "output_handling",
			Description: "Did the model correctly process and relay the agent's output?",
			Weight:      0.3,
			Required:    true,
		},
		{
			Name:        "error_recovery",
			Description: "On failure, did the model provide a useful explanation to the user?",
			Weight:      0.3,
		},
	},
}
