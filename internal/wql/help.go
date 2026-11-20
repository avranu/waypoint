package wql

import _ "embed"

// Help is the query language reference (markdown), served by the API at
// /api/v1/help/query and printed by `wp help query`.
//
//go:embed help.md
var Help string
