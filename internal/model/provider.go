package model

import "context"

type Role string

const (
	RoleUser      Role = "user"
	RoleSystem    Role = "system"
	RoleAssistant Role = "assistant"
)

type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	Messages    []Message `json:"messages"`
	Model       string    `json:"model"`
	Temperature float64   `json:"temperature,omitempty"`
}

type Response struct {
	Content string `json:"content"`
	Raw     []byte `json:"raw,omitempty"`
}

type Provider interface {
	Complete(ctx context.Context, request Request) (Response, error)
}
