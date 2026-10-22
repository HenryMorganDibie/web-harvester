#!/usr/bin/env python3
"""Offline regression tests for the Deal Intelligence discovery pilot."""
import unittest
import sys
from datetime import date
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from scripts.deal_intelligence_tavily_pilot import (
    africa_relevant,
    candidate_matches_database,
    classify_candidates,
    extract_event_date,
    fetch_database,
    freshness_status,
    is_social_platform,
    normalize_url,
    parse_article,
    parse_date,
    related_fund_matches,
)


class FakeResponse:
    def __init__(self, payload, status=200):
        self.payload = payload
        self.status_code = status

    def raise_for_status(self):
        if self.status_code >= 400:
            raise RuntimeError(f"HTTP {self.status_code}")

    def json(self):
        return self.payload


class FakeClient:
    def __init__(self, pages):
        self.pages = pages
        self.calls = []

    def get(self, url, params, headers):
        self.calls.append(params["offset"])
        page = self.pages.get(params["offset"])
        if page is None:
            return FakeResponse({}, status=500)
        return FakeResponse(page)


class DiscoveryPilotTests(unittest.TestCase):
    def test_normalize_url_removes_fragment_and_tracking_slash(self):
        self.assertEqual(
            normalize_url("https://Example.com/story/?utm_source=x#section"),
            "https://example.com/story",
        )

    def test_social_platforms_are_skipped_but_publisher_pages_are_kept(self):
        self.assertTrue(is_social_platform("https://www.linkedin.com/posts/example"))
        self.assertTrue(is_social_platform("https://m.facebook.com/story"))
        self.assertTrue(is_social_platform("https://instagram.com/p/example"))
        self.assertFalse(is_social_platform("https://techcabal.com/2026/10/22/funding-news"))

    def test_parse_dates_and_rejects_unknown_dates(self):
        self.assertEqual(parse_date("2026-10-22T10:15:00Z"), date(2026, 10, 22))
        self.assertEqual(freshness_status("2026-10-20", today=date(2026, 10, 23), days=7)[0], "recent")
        self.assertEqual(freshness_status("2025-12-01", today=date(2026, 10, 23), days=30)[0], "stale")
        self.assertEqual(freshness_status(None, today=date(2026, 10, 23))[0], "undated")

    def test_article_parser_uses_article_lead_and_publisher_date(self):
        html = """
        <html><head><title>Immobazyme raises funding</title>
        <meta property="article:published_time" content="2026-10-22T10:00:00Z"></head>
        <body><nav>Fintech Fintech Fintech</nav>
        <article><p>South African biotech company Immobazyme raised a funding round.</p>
        <p>The investment supports biologics manufacturing.</p></article>
        <aside><p>Fintech payments fintech loans fintech.</p></aside></body></html>
        """
        article = parse_article(html)
        self.assertIn("Immobazyme", article["title"])
        self.assertIn("biotech", article["lead"].lower())
        self.assertEqual(article["published_date"], "2026-10-22T10:00:00Z")
        self.assertNotIn("Fintech Fintech Fintech", article["lead"])

    def test_jsonld_article_body_and_date_are_used_when_meta_is_missing(self):
        html = """
        <html><head><title>Publisher page</title></head><body>
        <script type="application/ld+json">
        {"@context":"https://schema.org","@type":"NewsArticle",
         "headline":"Kenyan climate-tech startup raises $8 million",
         "datePublished":"2026-10-21T09:00:00Z",
         "articleBody":"A Kenyan climate-tech startup raised $8 million in a Series A round. The investment will expand operations across East Africa."}
        </script></body></html>
        """
        article = parse_article(html)
        self.assertIn("climate-tech", article["title"])
        self.assertIn("raised $8 million", article["lead"])
        self.assertEqual(article["published_date"], "2026-10-21T09:00:00Z")

    def test_database_pagination_requires_complete_snapshot(self):
        client = FakeClient({
            0: {"total_in_database": 3, "deals": [{"id": "1"}, {"id": "2"}]},
            2: {"total_in_database": 3, "deals": [{"id": "3"}]},
        })
        self.assertEqual(len(fetch_database("https://api.example", client, page_size=2)), 3)
        self.assertEqual(client.calls, [0, 2])

        partial = FakeClient({0: {"total_in_database": 3, "deals": [{"id": "1"}]}})
        with self.assertRaises(RuntimeError):
            fetch_database("https://api.example", partial, page_size=2)

    def test_duplicate_match_uses_production_matcher_and_marks_existing(self):
        existing = {"id": "old", "announced": "2026-10-20", "deal_type": "acquisition", "target": "Immobazyme"}
        candidate = {"id": "new", "announced": "2026-10-21", "deal_type": "acquisition", "target": "Immobazyme"}
        matcher = lambda a, b: a.get("target", "").casefold() == b.get("target", "").casefold()
        self.assertEqual(candidate_matches_database(candidate, [existing], matcher), [existing])
        rows = classify_candidates([{
            "deal": candidate,
            "freshness_status": "recent",
            "source": {"page_fetch_status": "fetched"},
        }], [existing], matcher)
        self.assertEqual(rows[0]["review_status"], "already_in_database")
        self.assertEqual(rows[0]["database_match"][0]["id"], "old")

    def test_event_date_overrides_recent_article_date(self):
        text = (
            "The manager's story index dates the first-close announcement to November 6, 2025 "
            "and the final-close announcement to August 26, 2026."
        )
        self.assertEqual(
            extract_event_date(text, reference_date=date(2026, 10, 22)),
            date(2026, 8, 26),
        )
        self.assertEqual(
            freshness_status(extract_event_date(text, reference_date=date(2026, 10, 22)),
                             today=date(2026, 10, 22), days=30)[0],
            "stale",
        )

    def test_non_african_transaction_is_rejected_even_from_africa_query(self):
        no_country_matches = lambda text: []
        deal = {"country": None, "countries": []}
        self.assertFalse(africa_relevant(
            deal,
            "Grand City Properties acquires a stake in LEG Immobilien SE",
            "Luxembourg property company announces a shareholding.",
            no_country_matches,
        ))
        self.assertTrue(africa_relevant(
            {"country": "NG", "countries": ["NG"]},
            "Ventures Platform Fund II reaches final close",
            "The Nigerian fund manager confirmed its final close.",
            no_country_matches,
        ))

    def test_related_fund_check_flags_same_series_but_not_another_series(self):
        candidate = {
            "deal_type": "fund_raise", "target": "Ventures Platform Fund II",
            "announced": "2026-08-26",
        }
        existing = [
            {"id": "fund2", "deal_type": "fund_raise", "target": "Ventures Platform Fund II",
             "announced": "2025-11-06"},
            {"id": "fund1", "deal_type": "fund_raise", "target": "Ventures Platform Fund I",
             "announced": "2026-08-26"},
        ]
        matches = related_fund_matches(candidate, existing)
        self.assertEqual([row["id"] for row in matches], ["fund2"])

    def test_only_publisher_dated_recent_nonduplicates_are_potentially_new(self):
        matcher = lambda a, b: False
        rows = classify_candidates([
            {"deal": {"id": "1"}, "freshness_status": "recent",
             "source": {"page_fetch_status": "fetched", "date_basis": "publisher_article"}},
            {"deal": {"id": "2"}, "freshness_status": "recent",
             "source": {"page_fetch_status": "fetched", "date_basis": "search_result"}},
            {"deal": {"id": "3"}, "freshness_status": "undated",
             "source": {"page_fetch_status": "fetched", "date_basis": "publisher_article"}},
            {"deal": {"id": "4"}, "freshness_status": "recent",
             "source": {"page_fetch_status": "fetch_failed", "date_basis": "publisher"}},
        ], [], matcher)
        self.assertEqual([r["review_status"] for r in rows], [
            "potentially_new_needs_human_review", "needs_date_confirmation",
            "not_eligible_as_new", "not_eligible_as_new"
        ])


if __name__ == "__main__":
    unittest.main(verbosity=2)
