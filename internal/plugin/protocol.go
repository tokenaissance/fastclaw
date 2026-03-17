package plugin

import "encoding/json"

// JSON-RPC 2.0 types for plugin communication over stdin/stdout.

// Request is a JSON-RPC 2.0 request sent to a plugin.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      int             `json:"id"`
}

// Response is a JSON-RPC 2.0 response from a plugin.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
	ID      int             `json:"id"`
}

// Notification is a JSON-RPC 2.0 notification (no ID) from a plugin.
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return e.Message }

// Standard JSON-RPC methods.
const (
	MethodInitialize    = "initialize"
	MethodShutdown      = "shutdown"
	MethodChannelSend   = "channel.send"
	MethodToolList      = "tool.list"
	MethodToolExecute   = "tool.execute"
	MethodHookFire      = "hook.fire"
	MethodMessageInbound = "message.inbound"
)

// InitializeParams is sent with the initialize method.
type InitializeParams struct {
	Config map[string]interface{} `json:"config"`
}

// ChannelSendParams is sent with channel.send.
type ChannelSendParams struct {
	ChatID string `json:"chatId"`
	Text   string `json:"text"`
}

// ToolListResult is returned from tool.list.
type ToolListResult struct {
	Tools []ToolDef `json:"tools"`
}

// ToolDef describes a tool provided by a plugin.
type ToolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters,omitempty"`
}

// ToolExecuteParams is sent with tool.execute.
type ToolExecuteParams struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args"`
}

// ToolExecuteResult is returned from tool.execute.
type ToolExecuteResult struct {
	Result string `json:"result"`
}

// InboundMessageParams is sent by channel plugins via message.inbound notifications.
type InboundMessageParams struct {
	Channel    string `json:"channel"`
	ChatID     string `json:"chatId"`
	UserID     string `json:"userId"`
	Text       string `json:"text"`
	PeerKind   string `json:"peerKind,omitempty"`
	SenderName string `json:"senderName,omitempty"`
}

// HookFireParams is sent with hook.fire.
type HookFireParams struct {
	Event string                 `json:"event"`
	Data  map[string]interface{} `json:"data,omitempty"`
}

// newRequest creates a JSON-RPC 2.0 request.
func newRequest(method string, params interface{}, id int) (*Request, error) {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return &Request{
		JSONRPC: "2.0",
		Method:  method,
		Params:  raw,
		ID:      id,
	}, nil
}
