package servertools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MCP is a client of one Model Context Protocol server over Streamable HTTP. It
// speaks the tools part of the protocol only: Anthropic's connector takes no
// more. A server that answers with a plain JSON body or with an event stream
// are both read.
type MCP struct {
	url, token string
	hc         *http.Client
	session    string
	version    string
	next       int
}

// MCPTool is a tool an MCP server offers.
type MCPTool struct {
	Name, Description string
	InputSchema       obj
}

const mcpProtocol = "2025-06-18"

// ConnectMCP opens a session with the server at url. token, when set, is sent as
// a bearer token.
func ConnectMCP(ctx context.Context, url, token string) (*MCP, error) {
	c := &MCP{url: url, token: token, hc: &http.Client{Timeout: 60 * time.Second}}
	res, err := c.rpc(ctx, "initialize", obj{
		"protocolVersion": mcpProtocol,
		"capabilities":    obj{},
		"clientInfo":      obj{"name": "intact", "version": "1"},
	}, true)
	if err != nil {
		return nil, err
	}
	c.version = str(asObj(res)["protocolVersion"])
	if c.version == "" {
		c.version = mcpProtocol
	}
	if _, err := c.rpc(ctx, "notifications/initialized", nil, false); err != nil {
		return nil, err
	}
	return c, nil
}

// ListTools returns every tool of the server, following its paging.
func (c *MCP) ListTools(ctx context.Context) ([]MCPTool, error) {
	var out []MCPTool
	cursor := ""
	for page := 0; page < 20; page++ {
		params := obj{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := c.rpc(ctx, "tools/list", params, true)
		if err != nil {
			return nil, err
		}
		r := asObj(res)
		for _, t := range list(r["tools"]) {
			td := asObj(t)
			name := str(td["name"])
			if name == "" {
				continue
			}
			s := asObj(td["inputSchema"])
			if s == nil {
				s = obj{"type": "object", "properties": obj{}}
			}
			out = append(out, MCPTool{Name: name, Description: str(td["description"]), InputSchema: s})
		}
		if cursor = str(r["nextCursor"]); cursor == "" {
			break
		}
	}
	return out, nil
}

// Call runs a tool and returns what it answered as text, and whether the tool
// reported an error. An error of the protocol or the transport is returned as an
// error.
func (c *MCP) Call(ctx context.Context, name string, args any) (string, bool, error) {
	if args == nil {
		args = obj{}
	}
	res, err := c.rpc(ctx, "tools/call", obj{"name": name, "arguments": args}, true)
	if err != nil {
		return "", false, err
	}
	r := asObj(res)
	var parts []string
	for _, item := range list(r["content"]) {
		it := asObj(item)
		switch str(it["type"]) {
		case "text":
			parts = append(parts, str(it["text"]))
		case "image", "audio":
			parts = append(parts, "["+str(it["type"])+" omitted]")
		case "resource":
			if t := str(asObj(it["resource"])["text"]); t != "" {
				parts = append(parts, t)
			}
		case "resource_link":
			parts = append(parts, str(it["uri"]))
		}
	}
	return strings.Join(parts, "\n"), isTrue(r["isError"]), nil
}

// rpc sends one JSON-RPC message. A notification (wantResult false) expects no
// answer; a request reads the answer with its id from the body or the stream.
func (c *MCP) rpc(ctx context.Context, method string, params any, wantResult bool) (any, error) {
	msg := obj{"jsonrpc": "2.0", "method": method}
	id := 0
	if wantResult {
		c.next++
		id = c.next
		msg["id"] = id
	}
	if params != nil {
		msg["params"] = params
	}
	b, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	if c.version != "" {
		req.Header.Set("MCP-Protocol-Version", c.version)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		c.session = s
	}
	if resp.StatusCode/100 != 2 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("MCP server answered %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	if !wantResult {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, nil
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		return readRPCStream(resp.Body, id)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	return rpcResult(raw, id)
}

// readRPCStream reads server-sent events until the answer to request id.
func readRPCStream(r io.Reader, id int) (any, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	var data []string
	flush := func() (any, bool, error) {
		if len(data) == 0 {
			return nil, false, nil
		}
		raw := strings.Join(data, "\n")
		data = nil
		res, err := rpcResult([]byte(raw), id)
		if err == errNotOurs {
			return nil, false, nil
		}
		return res, true, err
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if res, done, err := flush(); done {
				return res, err
			}
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(v, " "))
		}
	}
	if res, done, err := flush(); done {
		return res, err
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("MCP server closed the stream without an answer")
}

var errNotOurs = fmt.Errorf("not the answer")

// rpcResult reads a JSON-RPC response for request id. A message that is not that
// response (a notification, or the answer to another request) is errNotOurs.
func rpcResult(raw []byte, id int) (any, error) {
	m, err := decode(raw)
	if err != nil {
		return nil, fmt.Errorf("MCP server sent a body that is not JSON: %v", err)
	}
	if m["id"] == nil || number(m["id"]) != int64(id) {
		return nil, errNotOurs
	}
	if e := asObj(m["error"]); e != nil {
		return nil, fmt.Errorf("MCP error %d: %s", number(e["code"]), str(e["message"]))
	}
	return m["result"], nil
}
