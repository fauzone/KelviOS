package compose

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ValidateSpec validates a compose spec based on its kind.
func ValidateSpec(kind string, spec json.RawMessage) error {
	if len(spec) == 0 {
		return fmt.Errorf("spec is empty")
	}

	var raw map[string]any
	if err := json.Unmarshal(spec, &raw); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}

	switch kind {
	case KindReportSchedule:
		return validateReportScheduleSpec(raw)
	case KindAutomationRule:
		return validateAutomationRuleSpec(raw)
	case KindRecipe:
		return validateRecipeSpec(raw)
	case KindToolChain:
		return validateToolChainSpec(raw)
	default:
		return fmt.Errorf("unknown kind: %s", kind)
	}
}

func validateReportScheduleSpec(spec map[string]any) error {
	// Required: transport, chat_id, interval_hours
	transport, ok := spec["transport"]
	if !ok {
		return fmt.Errorf("report_schedule spec requires 'transport'")
	}
	transportStr, ok := transport.(string)
	if !ok || (transportStr != "telegram" && transportStr != "whatsapp") {
		return fmt.Errorf("'transport' must be 'telegram' or 'whatsapp'")
	}

	chatID, ok := spec["chat_id"]
	if !ok {
		return fmt.Errorf("report_schedule spec requires 'chat_id'")
	}
	chatIDStr, ok := chatID.(string)
	if !ok || strings.TrimSpace(chatIDStr) == "" {
		return fmt.Errorf("'chat_id' must be a non-empty string")
	}

	intervalHours, ok := spec["interval_hours"]
	if !ok {
		return fmt.Errorf("report_schedule spec requires 'interval_hours'")
	}
	intervalFloat, ok := intervalHours.(float64)
	if !ok {
		return fmt.Errorf("'interval_hours' must be a number")
	}
	interval := int(intervalFloat)
	if interval < 1 || interval > 168 {
		return fmt.Errorf("'interval_hours' must be between 1 and 168")
	}

	return nil
}

func validateAutomationRuleSpec(spec map[string]any) error {
	// Required: trigger, action
	trigger, ok := spec["trigger"]
	if !ok {
		return fmt.Errorf("automation_rule spec requires 'trigger'")
	}
	triggerMap, ok := trigger.(map[string]any)
	if !ok {
		return fmt.Errorf("'trigger' must be an object")
	}

	triggerType, ok := triggerMap["type"]
	if !ok {
		return fmt.Errorf("'trigger.type' is required")
	}
	triggerTypeStr, ok := triggerType.(string)
	if !ok || triggerTypeStr != "schedule" {
		return fmt.Errorf("'trigger.type' must be 'schedule'")
	}

	intervalHours, ok := triggerMap["interval_hours"]
	if !ok {
		return fmt.Errorf("'trigger.interval_hours' is required")
	}
	intervalFloat, ok := intervalHours.(float64)
	if !ok {
		return fmt.Errorf("'trigger.interval_hours' must be a number")
	}
	interval := int(intervalFloat)
	if interval < 1 || interval > 168 {
		return fmt.Errorf("'trigger.interval_hours' must be between 1 and 168")
	}

	action, ok := spec["action"]
	if !ok {
		return fmt.Errorf("automation_rule spec requires 'action'")
	}
	actionMap, ok := action.(map[string]any)
	if !ok {
		return fmt.Errorf("'action' must be an object")
	}

	actionType, ok := actionMap["type"]
	if !ok {
		return fmt.Errorf("'action.type' is required")
	}
	actionTypeStr, ok := actionType.(string)
	if !ok || actionTypeStr != "send_report" {
		return fmt.Errorf("'action.type' must be 'send_report'")
	}

	transport, ok := actionMap["transport"]
	if !ok {
		return fmt.Errorf("'action.transport' is required")
	}
	transportStr, ok := transport.(string)
	if !ok || (transportStr != "telegram" && transportStr != "whatsapp") {
		return fmt.Errorf("'action.transport' must be 'telegram' or 'whatsapp'")
	}

	chatID, ok := actionMap["chat_id"]
	if !ok {
		return fmt.Errorf("'action.chat_id' is required")
	}
	chatIDStr, ok := chatID.(string)
	if !ok || strings.TrimSpace(chatIDStr) == "" {
		return fmt.Errorf("'action.chat_id' must be a non-empty string")
	}

	return nil
}

func validateRecipeSpec(spec map[string]any) error {
	// Required: steps (array of objects with tool, args)
	steps, ok := spec["steps"]
	if !ok {
		return fmt.Errorf("recipe spec requires 'steps' array")
	}
	stepsArr, ok := steps.([]any)
	if !ok {
		return fmt.Errorf("'steps' must be an array")
	}
	if len(stepsArr) == 0 {
		return fmt.Errorf("'steps' must have at least one step")
	}

	for i, step := range stepsArr {
		stepMap, ok := step.(map[string]any)
		if !ok {
			return fmt.Errorf("step %d must be an object", i)
		}
		// Required: tool (non-empty string)
		tool, ok := stepMap["tool"]
		if !ok {
			return fmt.Errorf("step %d requires 'tool'", i)
		}
		toolStr, ok := tool.(string)
		if !ok || strings.TrimSpace(toolStr) == "" {
			return fmt.Errorf("step %d 'tool' must be a non-empty string", i)
		}
		// Required: args (object)
		args, ok := stepMap["args"]
		if !ok {
			return fmt.Errorf("step %d requires 'args'", i)
		}
		if _, ok := args.(map[string]any); !ok {
			return fmt.Errorf("step %d 'args' must be an object", i)
		}
	}
	return nil
}

func validateToolChainSpec(spec map[string]any) error {
	// Required: chain (array of non-empty strings)
	chain, ok := spec["chain"]
	if !ok {
		return fmt.Errorf("tool_chain spec requires 'chain' array")
	}
	chainArr, ok := chain.([]any)
	if !ok {
		return fmt.Errorf("'chain' must be an array")
	}
	if len(chainArr) == 0 {
		return fmt.Errorf("'chain' must have at least one element")
	}

	for i, item := range chainArr {
		itemStr, ok := item.(string)
		if !ok || strings.TrimSpace(itemStr) == "" {
			return fmt.Errorf("chain[%d] must be a non-empty string", i)
		}
	}
	return nil
}