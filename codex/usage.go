package codex

import (
	"encoding/json"

	"github.com/Luolc/redcoast/claude"
)

// usageEvents are the Responses API stream events whose response object the gateway
// reads: the first names the model, the last three end the response with its usage.
var usageEvents = map[string]bool{"response.created": true, "response.completed": true, "response.incomplete": true, "response.failed": true}

// usage reads one event of a Responses API stream: the model and the input and output
// token counts of its response object. The input count includes the cached tokens,
// which are not counted apart.
func usage(document []byte) claude.UsageReport {
	var event struct {
		Type     string `json:"type"`
		Response *struct {
			Model string `json:"model"`
			Usage *struct {
				InputTokens  *int64 `json:"input_tokens"`
				OutputTokens *int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(document, &event) != nil || !usageEvents[event.Type] || event.Response == nil {
		return claude.UsageReport{}
	}
	report := claude.UsageReport{Model: event.Response.Model}
	if u := event.Response.Usage; u != nil {
		report.InputTokens, report.OutputTokens = u.InputTokens, u.OutputTokens
	}
	return report
}
