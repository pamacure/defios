package node

import "defios.com/internal/serve"

// AgentTriggers is group's Trigger config as h gives it the
// Trigger-config Tools.
func AgentTriggers(h *Host, group string) serve.TriggerConfig {
	return agentTriggers{h: h, group: group}
}
