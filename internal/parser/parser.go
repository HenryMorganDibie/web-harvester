// Package parser extracts structured data from raw SERP HTML.
//
// The selectors here target the fixture HTML in testdata/, which is a
// simplified, generic stand-in for a real search results page (see
// testdata/full.html for why). Google's actual production markup is
// obfuscated and changes on its own schedule; retargeting these selectors
// at current live markup, and keeping them correct as Google ships layout
// changes, is real ongoing work — the point of this package is the
// extraction *architecture* (structured output, optional-block handling,
// drift detection via Calibration) that the retargeting work plugs into.
package parser

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/HenryMorganDibie/web-harvester/internal/model"
)

// Parser extracts a model.SerpResult from raw HTML.
type Parser struct{}

// New builds a Parser.
func New() *Parser {
	return &Parser{}
}

// Parse extracts structured data for query from html.
func (p *Parser) Parse(query string, html []byte) (*model.SerpResult, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(html))
	if err != nil {
		return nil, fmt.Errorf("parser: parse html: %w", err)
	}

	result := &model.SerpResult{Query: query}
	var missing []string

	// Keep requested terms, or use the actual query shown in the search input.
	result.SearchTerms = strings.Fields(query)
	if q, ok := doc.Find(`input[name="q"]`).Attr("value"); ok && strings.TrimSpace(q) != "" {
		result.SearchTerms = strings.Fields(strings.TrimSpace(q))
	}

	// Capture source cards and visible disclosure text when selectors are present.
	doc.Find(".source-card, [data-source-card], .ai-overview-source, .MjjYud").Each(func(_ int, s *goquery.Selection) {
		a := s.Find("a").First()
		title := strings.TrimSpace(a.Text())
		href, _ := a.Attr("href")
		desc := strings.TrimSpace(s.Find(".VwiC3b, .description, [data-sncf], .snippet").First().Text())
		if title != "" || href != "" || desc != "" {
			result.SourceCards = append(result.SourceCards, model.SourceCard{Title: title, URL: href, Description: desc})
		}
	})
	doc.Find("[aria-expanded='true'], [data-expanded='true'], details[open]").Each(func(_ int, s *goquery.Selection) {
		t := strings.TrimSpace(s.Text())
		if t != "" { result.ExpandableTexts = append(result.ExpandableTexts, t) }
	})

	if ov := doc.Find(".serp-ai-overview"); ov.Length() > 0 {
		text := strings.TrimSpace(ov.Find(".ai-overview-text").Text())
		var sources []string
		ov.Find(".ai-overview-sources a").Each(func(_ int, s *goquery.Selection) {
			if href, ok := s.Attr("href"); ok && href != "" {
				sources = append(sources, href)
				title := strings.TrimSpace(s.Text())
				if title != "" { result.SourceCards = append(result.SourceCards, model.SourceCard{Title: title, URL: href}) }
			}
		})
		result.AIOverview = &model.AIOverview{Text: text, Sources: sources}
	}

	if fs := doc.Find(".serp-featured-snippet"); fs.Length() > 0 {
		href, _ := fs.Find(".fs-link").Attr("href")
		result.FeaturedSnippet = &model.FeaturedSnippet{
			Title: strings.TrimSpace(fs.Find(".fs-title").Text()),
			URL:   href,
			Text:  strings.TrimSpace(fs.Find(".fs-text").Text()),
		}
	}

	doc.Find(".paa-question").Each(func(_ int, s *goquery.Selection) {
		q := strings.TrimSpace(s.Text())
		if q != "" {
			result.PeopleAlsoAsk = append(result.PeopleAlsoAsk, q)
		}
	})

	doc.Find(".organic-result").Each(func(i int, s *goquery.Selection) {
		href, _ := s.Find(".organic-link").Attr("href")
		result.Organic = append(result.Organic, model.OrganicResult{
			Position: i + 1,
			Title:    strings.TrimSpace(s.Find(".organic-title").Text()),
			URL:      href,
			Snippet:  strings.TrimSpace(s.Find(".organic-snippet").Text()),
		})
	})

	// Organic results are the one block we always expect. Its absence means
	// the selectors no longer match this page's layout, not that the page
	// genuinely has zero results — flag it rather than returning an empty
	// success.
	if len(result.Organic) == 0 {
		missing = append(missing, "organic_results")
	}

	if len(missing) > 0 {
		result.Calibration = &model.CalibrationNote{MissingBlocks: missing}
	}

	return result, nil
}
