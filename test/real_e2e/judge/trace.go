package judge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Trace struct {
	TestName    string `json:"test_name"`
	UserMessage string `json:"user_message"`
	Turns       []Turn `json:"turns"`
}

type Turn struct {
	ModelOutput string             `json:"model_output"`
	ToolCalls   []ToolCallRecord   `json:"tool_calls,omitempty"`
	ToolResults []ToolResultRecord `json:"tool_results,omitempty"`
	Duration    time.Duration      `json:"duration"`
}

type ToolCallRecord struct {
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input,omitempty"`
}

type ToolResultRecord struct {
	ToolName string `json:"tool_name"`
	Output   string `json:"output"`
	IsError  bool   `json:"is_error"`
}

func CollectTrace(testName, userMsg string, rd *bufio.Reader, timeout time.Duration) (*Trace, error) {
	trace := &Trace{
		TestName:    testName,
		UserMessage: userMsg,
	}
	var turn Turn
	start := time.Now()
	deadline := time.Now().Add(timeout)
	var curEvent string

	for time.Now().Before(deadline) {
		line, err := rd.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}

		if strings.HasPrefix(line, "event: ") {
			curEvent = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		switch curEvent {
		case "assistant_text_delta":
			var d map[string]any
			if json.Unmarshal([]byte(data), &d) == nil {
				if delta, ok := d["delta"].(string); ok {
					turn.ModelOutput += delta
				}
			}
		case "assistant_text_done":
			turn.Duration = time.Since(start)
			trace.Turns = append(trace.Turns, turn)
			turn = Turn{}
			start = time.Now()
			var d map[string]any
			if json.Unmarshal([]byte(data), &d) == nil {
				if sr, ok := d["stop_reason"].(string); ok && sr == "end_turn" {
					return trace, nil
				}
			}
		case "tool_call_start":
			var d map[string]any
			if json.Unmarshal([]byte(data), &d) == nil {
				name, _ := d["tool"].(string)
				turn.ToolCalls = append(turn.ToolCalls, ToolCallRecord{
					ToolName: name,
				})
			}
		case "tool_call_done":
			var d map[string]any
			if json.Unmarshal([]byte(data), &d) == nil {
				name, _ := d["tool"].(string)
				output, _ := d["output"].(string)
				isErr := false
				if v, ok := d["ok"].(bool); ok {
					isErr = !v
				}
				turn.ToolResults = append(turn.ToolResults, ToolResultRecord{
					ToolName: name,
					Output:   output,
					IsError:  isErr,
				})
			}
		case "error":
			return trace, nil
		}
	}

	if turn.ModelOutput != "" || len(turn.ToolCalls) > 0 || len(turn.ToolResults) > 0 {
		turn.Duration = time.Since(start)
		trace.Turns = append(trace.Turns, turn)
	}
	if len(trace.Turns) == 0 {
		return trace, fmt.Errorf("trace collection timed out after %v with no complete turns", timeout)
	}
	return trace, nil
}
