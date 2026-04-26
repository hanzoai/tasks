// Copyright © 2026 Hanzo AI. MIT License.

package workflow

// UpsertSearchAttributes upserts the supplied attribute map into the
// running workflow's visibility record. Mirrors
// go.temporal.io/sdk/workflow.UpsertSearchAttributes — keys are user-
// defined search attribute names registered on the namespace; values
// are encoded by the SQL visibility store.
//
// This is the ONE workflow-side primitive callers in service/worker
// consume for search-attribute propagation; the typed and namespace-
// scoped variants from upstream are not used here and are not added.
func UpsertSearchAttributes(ctx Context, attrs map[string]any) error {
	if ctx == nil {
		return errNilContext
	}
	return ctx.env().UpsertSearchAttributes(attrs)
}
