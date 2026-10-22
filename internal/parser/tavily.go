package parser

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/HenryMorganDibie/web-harvester/internal/model"
)

type tavilyResponse struct {
	Results []struct {
		Title string `json:"title"`
		URL string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
	Answer string `json:"answer"`
}

// TavilyParser maps Tavily's native response into the shared SerpResult model.
// Google-specific fields such as People Also Ask and AI Overviews remain absent.
type TavilyParser struct{}

func NewTavily() *TavilyParser { return &TavilyParser{} }

func (p *TavilyParser) Parse(query string, body []byte) (*model.SerpResult, error) {
	var raw tavilyResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("tavily parser: decode response: %w", err)
	}
	result := &model.SerpResult{Query: query}
	for i, item := range raw.Results {
		if strings.TrimSpace(item.Title) == "" && strings.TrimSpace(item.URL) == "" && strings.TrimSpace(item.Content) == "" { continue }
		result.Organic = append(result.Organic, model.OrganicResult{Position: i + 1, Title: item.Title, URL: item.URL, Snippet: item.Content})
	}
	if strings.TrimSpace(raw.Answer) != "" {
		result.FeaturedSnippet = &model.FeaturedSnippet{Title: "Tavily answer", Text: raw.Answer}
	}
	if len(result.Organic) == 0 {
		result.Calibration = &model.CalibrationNote{MissingBlocks: []string{"organic_results"}}
	}
	return result, nil
}
