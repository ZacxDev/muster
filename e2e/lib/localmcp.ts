// A mirrored host's loopback MCP endpoint (muster-mirror -mcp-listen) holds a
// Claude Code caller to an MCP session while its tool watch is on (the
// default): `initialize` issues an Mcp-Session-Id and every later request must
// carry it, or it is the transport's 400 (none) or 404 (unknown) —
// docs/mirroring.md, "The local MCP endpoint". withMCPSession initializes as
// the caller `headers` name and answers them with that id. When none is
// issued (a refused caller, a caller from another tmux server, or the watch
// off), or the endpoint is not up yet, they are answered unchanged.
export async function withMCPSession(url: string, headers: Record<string, string>): Promise<Record<string, string>> {
  try {
    const r = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream', ...headers },
      body: JSON.stringify({
        jsonrpc: '2.0',
        id: 0,
        method: 'initialize',
        params: { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'e2e', version: '0' } },
      }),
    });
    await r.text();
    const id = r.headers.get('mcp-session-id');
    return id ? { ...headers, 'Mcp-Session-Id': id } : headers;
  } catch {
    return headers;
  }
}
