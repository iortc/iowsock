package iowsock

import "time"

const (
	HeartbeatCheck   = 30 * time.Second
	HeartbeatTimeout = 60 * time.Second
	BufferLevel      = 64
)

type EventMessage struct {
	Event string                 `json:"event"`
	Body  map[string]interface{} `json:"body,omitempty"`
}
