// Command echoserver is a minimal MCP stdio server used in integration tests.
// It responds to initialize, tools/list, and tools/call. The single "echo" tool
// returns the input arguments as its output.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}

		var req map[string]interface{}
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}

		method, _ := req["method"].(string)
		id, hasID := req["id"]

		// Skip notifications (no id) — they need no response.
		if !hasID {
			continue
		}

		switch method {
		case "initialize":
			writeResp(id, map[string]interface{}{
				"protocolVersion": "2025-11-25",
				"capabilities":    map[string]interface{}{},
				"serverInfo": map[string]interface{}{
					"name":    "echoserver",
					"version": "1.0.0",
				},
			})
		case "tools/list":
			writeResp(id, map[string]interface{}{
				"tools": []map[string]interface{}{
					{
						"name":        "echo",
						"description": "Echoes the input arguments back to the caller.",
						"inputSchema": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"message": map[string]interface{}{
									"type": "string",
								},
							},
						},
					},
				},
			})
		case "tools/call":
			writeResp(id, map[string]interface{}{
				"content": []map[string]interface{}{
					{"type": "text", "text": formatArgs(req)},
				},
			})
		}
	}
}

func formatArgs(req map[string]interface{}) string {
	params, _ := req["params"].(map[string]interface{})
	args, _ := params["arguments"].(map[string]interface{})
	if args == nil {
		return "echo: (no args)"
	}
	b, _ := json.Marshal(args)
	return "echo: " + string(b)
}

func writeResp(id interface{}, result interface{}) {
	var idFloat float64
	switch v := id.(type) {
	case float64:
		idFloat = v
	default:
		return
	}
	resp := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      int64(idFloat),
		"result":  result,
	}
	b, _ := json.Marshal(resp)
	fmt.Println(string(b))
}
