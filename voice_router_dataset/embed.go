// Package dataset embeds the fixed business catalog and synthetic backend.
// Evaluation utterances and labels are deliberately not embedded in the router.
package dataset

import "embed"

//go:embed scenarios.json slots.json actions.json knowledge_base.json mock_backend.json
var Files embed.FS
