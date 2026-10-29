package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *srv) addPrompts() {
	s.server.AddPrompt(&mcp.Prompt{
		Name:  "prospecting_campaign",
		Title: "Prospecting campaign",
		Description: "From an activity and an area to a checked, exported lead list: create the job, follow it, " +
			"control quality, export.",
		Arguments: []*mcp.PromptArgument{
			{Name: "activity", Title: "Activity", Description: "e.g. plombier, restaurant, or a sector key (" + strings.Join(sectorKeys()[:5], ", ") + "...)", Required: true},
			{Name: "area", Title: "Area", Description: "place name, e.g. 'Lyon, France' or 'Dubai Marina'", Required: true},
			{Name: "country", Description: "ISO code when the place name is ambiguous (optional)"},
			{Name: "language", Description: "language of categories and reviews, default fr"},
			{Name: "emails", Description: "yes to crawl websites for emails (slower)"},
			{Name: "max_results", Description: "exact quota of businesses (optional)"},
		},
	}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		a := req.Params.Arguments
		opt := func(k, label string) string {
			if v := strings.TrimSpace(a[k]); v != "" {
				return fmt.Sprintf("\n- %s: %s", label, v)
			}
			return ""
		}
		text := fmt.Sprintf(`Run a prospecting campaign with the CLEADS tools.

Target:
- activity: %s
- area: %s%s%s%s%s

Steps:
1. If the activity matches a key of list_sectors, pass it as sector; otherwise pass it as query (in the job language).
2. create_extraction_job with exactly one area form (location here). Set enrich to true only if emails are wanted. Keep the returned idempotency_key and reuse it if you must retry the call.
3. wait_for_job until finished=true (call it again when timed_out=true). Report progress briefly; partial results are readable with get_job_results meanwhile.
4. Quality control: report the counters (unique, complete, crm_useful, enriched). Read one compact page of get_job_results with useful=true and check that the records match the activity and the area. If tasks failed, call get_job_errors and summarize the causes.
5. export_job (csv-excel for Excel users) and give the download URL and resource link. Do not page through every record in the conversation.`,
			a["activity"], a["area"], opt("country", "country"), opt("language", "language"), opt("emails", "emails wanted"), opt("max_results", "max_results"))
		return &mcp.GetPromptResult{Description: "Prospecting campaign: " + a["activity"] + " in " + a["area"],
			Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}}}, nil
	})

	s.server.AddPrompt(&mcp.Prompt{
		Name:        "diagnose_job",
		Title:       "Diagnose a job",
		Description: "Find out why a job failed, is slow or returned few results.",
		Arguments:   []*mcp.PromptArgument{{Name: "job_id", Title: "Job id", Description: "job_...", Required: true}},
	}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		text := fmt.Sprintf(`Diagnose CLEADS job %s.

1. get_job: status, counters, error. Few results with no failure usually means a small area, a narrow query, or unrelated places filtered out (include_unrelated).
2. get_job_errors: failures grouped by cause. Name the top causes with their counts and sources.
3. get_system_health: is a source blocked (circuit open, 403/captcha), throttled (429) or failing? Are workers alive and is the queue moving?
4. If the API key is the admin key, get_logs with job_id (and source when one source stands out) for the detailed messages.
5. Conclude: the probable cause, whether it is transient (retry later, lower SOURCE_RATE, add proxies) or a request problem (area, country, sector), and the next action.`, req.Params.Arguments["job_id"])
		return &mcp.GetPromptResult{Description: "Diagnose job " + req.Params.Arguments["job_id"],
			Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}}}, nil
	})
}
