package bridge

import "fmt"

// Error is the JSON-RPC 2.0 error envelope, exported so clients can
// type-assert on Code rather than parse messages.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("bridge: %d %s", e.Code, e.Message)
}

// Reserved codes (docs/bridge.md §6). The -327xx range is the JSON-RPC
// standard; -320xx is the bridge's own reserved block.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	CodeNotInitialised = -32000
	CodeNoMachine      = -32001
	CodeUnknownPreset  = -32002
	CodeBusError       = -32003
	CodeImageReject    = -32004
	CodeUnknownBP      = -32005
	CodeCapMissing     = -32006
	CodeNotInRun       = -32007
)

func newErr(code int, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

func newErrData(code int, msg string, d any) *Error {
	return &Error{Code: code, Message: msg, Data: d}
}
