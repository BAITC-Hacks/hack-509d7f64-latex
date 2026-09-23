package router

import (
 "context"
 "errors"
)

var (
 ErrBusy = errors.New("session is already processing a turn")
 ErrNotFound = errors.New("record not found")
 ErrDatabase = errors.New("database unavailable")
)

type IntentReview struct {
 ProposalID string `json:"proposal_id"`
 Revision int `json:"revision"`
 Target string `json:"target"`
 Question string `json:"question"`
 Decision Decision `json:"decision"`
 Status string `json:"status"`
}
type ReviewInput struct {
 RequestID string `json:"request_id"`
 TurnRequestID string `json:"turn_request_id"`
 ProposalID string `json:"proposal_id"`
 Revision int `json:"revision"`
 Decision string `json:"decision"`
 Feedback string `json:"feedback,omitempty"`
}
type ReviewReceipt struct {
 Input ReviewInput `json:"input"`
 Actor string `json:"actor"`
 Output *Output `json:"output,omitempty"`
}
// Run is the durable checkpoint of a single user input. Session.Turns is a
// read projection, never nested inside persisted session JSON.
type Run struct {
 Input Input `json:"input"`
 Phase string `json:"phase"`
 Output Output `json:"output"`
 Decision *Decision `json:"decision,omitempty"`
 Attempts int `json:"attempts"`
 Steps int `json:"steps"`
 ActiveMS int64 `json:"active_ms"`
 FrameReady bool `json:"frame_ready"`
 ActionApproved bool `json:"action_approved"`
 LowCounted bool `json:"low_counted"`
 Review *IntentReview `json:"review,omitempty"`
 Reviews []ReviewReceipt `json:"reviews,omitempty"`
 RoutingContext []Values `json:"routing_context,omitempty"`
 Rejected []string `json:"rejected,omitempty"`
 ToolSerial int `json:"tool_serial"`
}
type Event struct { Kind string `json:"kind"`; Data Values `json:"data"` }

type Repository interface {
 Lock(context.Context,string)(Lease,error)
 Get(context.Context,string)(Session,error)
 GetTurn(context.Context,string,string)(Run,error)
 Ready(context.Context) error
}
// A lease owns one PostgreSQL connection and its session advisory lock. No SQL
// transaction is held while calling a model or waiting for a human.
type Lease interface {
	Context() context.Context
 Load(context.Context)(Session,error)
 Begin(context.Context,Input)(Run,bool,error)
 GetRun(context.Context,string)(Run,error)
 Save(context.Context,*Session,*Run,*Event) error
 // Tool commits synthetic state, receipt, and the apply callback's checkpoint
 // in one transaction. A receipt replay returns its saved result to apply.
 Tool(context.Context,*Session,*Run,string,string,Values,func(Values)) error
 Release()
}
