package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
)

// ToolDefinition represents a tool advertised by an MCP server.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ContentItem is one element of a tool call result.
type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// CallToolResult is the payload returned by tools/call.
type CallToolResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// Client manages a connection to a single stdio MCP server.
type Client struct {
	ServerName string
	Command    string
	Args       []string
	Env        []string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
	reqID  atomic.Int64
}

// NewClient prepares an MCP client over stdio.
func NewClient(name, command string, args []string, env []string) *Client {
	return &Client{
		ServerName: name,
		Command:    command,
		Args:       args,
		Env:        env,
	}
}

// Start launches the server process and initializes the protocol.
func (c *Client) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cmd != nil {
		return nil
	}

	cmd := exec.CommandContext(ctx, c.Command, c.Args...)
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("mcp [%s]: stdin pipe: %w", c.ServerName, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return fmt.Errorf("mcp [%s]: stdout pipe: %w", c.ServerName, err)
	}
	cmd.Stderr = os.Stderr // Pass through diagnostic logs

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return fmt.Errorf("mcp [%s]: start %s: %w", c.ServerName, c.Command, err)
	}

	c.cmd = cmd
	c.stdin = stdin
	c.stdout = bufio.NewReader(stdout)

	// Send initialize request
	initParams := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "boop",
			"version": "0.1.0",
		},
	}

	var initRes map[string]any
	if err := c.requestLocked(ctx, "initialize", initParams, &initRes); err != nil {
		c.closeLocked()
		return fmt.Errorf("mcp [%s]: initialize: %w", c.ServerName, err)
	}

	// Send initialized notification
	notify := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}
	notifyBytes, _ := json.Marshal(notify)
	notifyBytes = append(notifyBytes, '\n')
	_, _ = c.stdin.Write(notifyBytes)

	return nil
}

// ListTools discovers tools offered by the server.
func (c *Client) ListTools(ctx context.Context) ([]ToolDefinition, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cmd == nil {
		return nil, errors.New("mcp client is not running")
	}

	var res struct {
		Tools []ToolDefinition `json:"tools"`
	}
	if err := c.requestLocked(ctx, "tools/list", map[string]any{}, &res); err != nil {
		return nil, fmt.Errorf("mcp [%s]: list tools: %w", c.ServerName, err)
	}
	return res.Tools, nil
}

// CallTool executes a tool on the server.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*CallToolResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cmd == nil {
		return nil, errors.New("mcp client is not running")
	}

	params := map[string]any{
		"name":      name,
		"arguments": args,
	}

	var res CallToolResult
	if err := c.requestLocked(ctx, "tools/call", params, &res); err != nil {
		return nil, fmt.Errorf("mcp [%s]: call %s: %w", c.ServerName, name, err)
	}
	return &res, nil
}

// Close terminates the server process.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeLocked()
}

func (c *Client) closeLocked() error {
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
		c.cmd = nil
	}
	return nil
}

func (c *Client) requestLocked(ctx context.Context, method string, params any, result any) error {
	id := c.reqID.Add(1)
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return err
	}
	reqBytes = append(reqBytes, '\n')

	if _, err := c.stdin.Write(reqBytes); err != nil {
		return err
	}

	// Read line response
	for {
		lineBytes, err := c.stdout.ReadBytes('\n')
		if err != nil {
			return err
		}
		if len(lineBytes) == 0 {
			continue
		}

		var rpcRes struct {
			ID     int64           `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(lineBytes, &rpcRes); err != nil {
			continue // Skip non-JSON lines or server log lines
		}
		if rpcRes.ID != id {
			continue
		}

		if rpcRes.Error != nil {
			return fmt.Errorf("server error (%d): %s", rpcRes.Error.Code, rpcRes.Error.Message)
		}

		if result != nil && len(rpcRes.Result) > 0 {
			return json.Unmarshal(rpcRes.Result, result)
		}
		return nil
	}
}
