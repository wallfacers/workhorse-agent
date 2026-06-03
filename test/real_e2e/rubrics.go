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

var grepRubric = judge.Rubric{
	MinScore:   0.7,
	MaxRetries: 2,
	Criteria: []judge.Criterion{
		{
			Name:        "grep_called",
			Description: "Did the model call the Grep tool with a correct pattern to search for the requested content?",
			Weight:      0.3,
			Required:    true,
		},
		{
			Name:        "results_accurate",
			Description: "Did the model accurately report the grep results — correct file names, line numbers, and matched content?",
			Weight:      0.4,
			Required:    true,
		},
		{
			Name:        "no_hallucination",
			Description: "Did the model avoid fabricating matches that are not in the grep output?",
			Weight:      0.2,
			Required:    true,
		},
		{
			Name:        "efficiency",
			Description: "Did the model avoid unnecessary extra tool calls (e.g., redundant reads after a successful grep)?",
			Weight:      0.1,
		},
	},
}

var grepNoMatchRubric = judge.Rubric{
	MinScore:   0.7,
	MaxRetries: 2,
	Criteria: []judge.Criterion{
		{
			Name:        "grep_called",
			Description: "Did the model call the Grep tool to search for the specified pattern?",
			Weight:      0.3,
			Required:    true,
		},
		{
			Name:        "no_match_reported",
			Description: "Did the model correctly report that no matches were found, without fabricating results?",
			Weight:      0.5,
			Required:    true,
		},
		{
			Name:        "helpful_response",
			Description: "Did the model provide a helpful response when no matches were found (e.g., suggesting alternative patterns)?",
			Weight:      0.2,
		},
	},
}

var todoWriteRubric = judge.Rubric{
	MinScore:   0.7,
	MaxRetries: 2,
	Criteria: []judge.Criterion{
		{
			Name:        "tool_invocation",
			Description: "Did the model call the TodoWrite tool with a valid tasks array containing the expected items?",
			Weight:      0.3,
			Required:    true,
		},
		{
			Name:        "task_accuracy",
			Description: "Are the task subjects and descriptions accurate and matching what the user requested?",
			Weight:      0.3,
			Required:    true,
		},
		{
			Name:        "status_correct",
			Description: "Are the task statuses correct (pending for new tasks, completed for done tasks, in_progress for active)?",
			Weight:      0.2,
			Required:    true,
		},
		{
			Name:        "complete_replacement",
			Description: "Did the model pass the complete task list each time (not just the delta), as required by the tool spec?",
			Weight:      0.2,
		},
	},
}

var complexWorkflowRubric = judge.Rubric{
	MinScore:   0.65,
	MaxRetries: 2,
	Criteria: []judge.Criterion{
		{
			Name:        "multi_tool_sequence",
			Description: "Did the model use the correct sequence of tools to accomplish the multi-step task?",
			Weight:      0.25,
			Required:    true,
		},
		{
			Name:        "data_flow_correct",
			Description: "Is data correctly passed between tool calls (e.g., grep results inform the next action)?",
			Weight:      0.3,
			Required:    true,
		},
		{
			Name:        "final_output_accurate",
			Description: "Is the final output (written file, task list) accurate and complete?",
			Weight:      0.3,
			Required:    true,
		},
		{
			Name:        "no_unnecessary_steps",
			Description: "Did the model avoid unnecessary tool calls or redundant operations?",
			Weight:      0.15,
		},
	},
}

var toolSearchRubric = judge.Rubric{
	MinScore:   0.7,
	MaxRetries: 2,
	Criteria: []judge.Criterion{
		{
			Name:        "discovered_via_search",
			Description: "The deferred tool's full schema is not provided upfront — its name only appears in an <available-deferred-tools> announcement. Did the model first call the ToolSearch tool to discover/load the relevant deferred tool (e.g. query 'weather' or select:weather__forecast) before being able to use it?",
			Weight:      0.4,
			Required:    true,
		},
		{
			Name:        "deferred_tool_called",
			Description: "After discovering it via ToolSearch, did the model then call the actual deferred tool (e.g. weather__forecast) with appropriate parameters to satisfy the user's request?",
			Weight:      0.35,
			Required:    true,
		},
		{
			Name:        "answer_reflects_result",
			Description: "Does the final answer accurately reflect the deferred tool's output without fabrication?",
			Weight:      0.25,
			Required:    true,
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
