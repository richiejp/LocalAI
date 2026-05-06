package localaitools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerMiddlewareTools wires the routing-module admin surface for the
// MCP server. The two tools mirror what the React /app/middleware page
// exposes:
//
//   - get_middleware_status: read-only aggregator. The agent can ask
//     "what's filtering my requests?" and get back the active PII
//     pattern set, the per-model resolved enabled/override state, and
//     a placeholder for routing.
//   - set_pii_pattern_action: mutating. Mutations are TRANSIENT — they
//     live until process restart, when patterns reload from the YAML
//     defaults. The skill prompt should warn the user about that
//     before applying lasting changes.
func registerMiddlewareTools(s *mcp.Server, client LocalAIClient, opts Options) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        ToolGetMiddlewareStatus,
		Description: "Aggregated routing-module status: PII pattern catalogue with current actions, per-model resolved PII state and overrides, recent event count, plus the active router models and their classifier configs. Read-only.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		status, err := client.GetMiddlewareStatus(ctx)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return jsonResult(status), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        ToolGetRouterDecisions,
		Description: "Recent intelligent-routing decisions. Each row records which router model the client called, which candidate the classifier picked, the classifier's score and latency, and a correlation id that joins back to the usage record. Filter by correlation_id, user_id, or router_model. Read-only.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args RouterDecisionsQuery) (*mcp.CallToolResult, any, error) {
		decisions, err := client.GetRouterDecisions(ctx, args)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return jsonResult(decisions), nil, nil
	})

	if opts.DisableMutating {
		return
	}

	mcp.AddTool(s, &mcp.Tool{
		Name:        ToolSetPIIPatternAction,
		Description: "Change a PII pattern's action (mask|block|route_local) in-process. TRANSIENT: the change is lost on restart. To persist, edit --pii-config YAML and restart. Admin-required.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args PIIPatternActionUpdate) (*mcp.CallToolResult, any, error) {
		if args.ID == "" {
			return errorResultf("id is required"), nil, nil
		}
		if args.Action == "" {
			return errorResultf("action is required (mask, block, or route_local)"), nil, nil
		}
		if err := client.SetPIIPatternAction(ctx, args); err != nil {
			return errorResult(err), nil, nil
		}
		return jsonResult(map[string]any{
			"id":        args.ID,
			"action":    args.Action,
			"persisted": false,
		}), nil, nil
	})
}
