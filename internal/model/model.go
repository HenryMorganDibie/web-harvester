// Package model defines the structured result of harvesting a single SERP.
package model

import "time"

// SerpResult is the structured output produced for one query.
type SerpResult struct {
	Query           string           `json:"query"`
	FinalURL        string           `json:"final_url,omitempty"`
	SearchTerms     []string         `json:"search_terms,omitempty"`
	SourceCards     []SourceCard     `json:"source_cards,omitempty"`
	ExpandableTexts []string         `json:"expandable_texts,omitempty"`
	RunID           string           `json:"run_id,omitempty"`
	Locale          string           `json:"locale,omitempty"`
	Device          string           `json:"device,omitempty"`
	FetchedAt       time.Time        `json:"fetched_at"`
	LatencyMS       int64            `json:"latency_ms"`
	ProxyUsed       string           `json:"proxy_used,omitempty"`
	AIOverview      *AIOverview      `json:"ai_overview,omitempty"`
	FeaturedSnippet *FeaturedSnippet `json:"featured_snippet,omitempty"`
	PeopleAlsoAsk   []string         `json:"people_also_ask,omitempty"`
	Organic         []OrganicResult  `json:"organic"`

	// Calibration is set when the parser could not confidently locate one or
	// more expected blocks. Production selector drift (Google changes its
	// markup frequently) should route these results to a review queue rather
	// than silently returning partial/empty data.
	Calibration *CalibrationNote `json:"calibration,omitempty"`

	// RawBody is populated only when Calibration is set — the raw, pre-parse
	// response body, for whoever is retuning selectors. Left nil otherwise
	// to keep normal results small; JSONLSink writes it as-is (base64 in
	// JSON), PostgresSink stores it in a dedicated column.
	RawBody []byte `json:"raw_body,omitempty"`
}

// OrganicResult is a single organic listing.
type OrganicResult struct {
	Position int    `json:"position"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Snippet  string `json:"snippet"`
}

// FeaturedSnippet is the "position zero" answer box, when present.
type FeaturedSnippet struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Text  string `json:"text"`
}

// AIOverview is Google's generative summary block, when present.
type AIOverview struct {
	Text    string   `json:"text"`
	Sources []string `json:"sources"`
}

// CalibrationNote flags that one or more expected selectors did not match,
// so this result should not be trusted as complete.
type CalibrationNote struct {
	MissingBlocks []string `json:"missing_blocks"`
	RawHTMLPath   string   `json:"raw_html_path,omitempty"`
}

// SourceCard captures visible source-card metadata when available.
type SourceCard struct {
	Title       string `json:"title,omitempty"`
	URL         string `json:"url,omitempty"`
	Description string `json:"description,omitempty"`
}
