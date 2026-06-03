# mcp-tool-search

借鉴 Claude Code 的 Tool Search：把可延迟工具（MCP 等）的 schema 从初始 tool 列表中移除，仅保留名字，由 `ToolSearch` 工具按需取回。客户端模拟实现（provider 无关），不依赖 Anthropic `tool_reference` beta。
