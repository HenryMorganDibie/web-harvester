#!/usr/bin/env python3
"""Recent-deal discovery with source validation and read-only production deduplication."""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
import time
from datetime import date, datetime, timedelta, timezone
from email.utils import parsedate_to_datetime
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import parse_qsl, urlencode, urlparse, urlunparse

import httpx

USER_AGENT = "DealIntelligenceDiscoveryPilot/2.0"
API_DEFAULT = "https://deal-intelligence-api-henry-dibie-s-projects.vercel.app"
QUERY_TEMPLATES = [
    "Africa startup funding round investment raises venture capital",
    "African private equity acquisition buyout stake transaction",
    "Africa private capital fund first close final close fundraising",
    "Africa DFI investment financing loan facility company",
    "African company acquisition merger stake investment announced",
    "Afrique startup levée de fonds acquisition investissement capital",
    "África empresa financiamento investimento aquisição capital de risco",
    "African company receives investment from IFC BII Proparco AfDB",
]
MAX_RESULTS_PER_QUERY = 10
FRESHNESS_DAYS = 30
PAGE_TIMEOUT = httpx.Timeout(20.0, connect=10.0)


def normalize_url(url: str) -> str:
    """Remove tracking fragments while preserving meaningful query parameters."""
    try:
        p = urlparse(url.strip())
        if p.scheme not in {"http", "https"} or not p.hostname:
            return ""
        host = p.hostname.lower()
        if p.port and not ((p.scheme == "http" and p.port == 80) or (p.scheme == "https" and p.port == 443)):
            host += f":{p.port}"
        path = re.sub(r"/{2,}", "/", p.path or "/")
        query = urlencode([
            (k, v) for k, v in parse_qsl(p.query, keep_blank_values=True)
            if not k.lower().startswith("utm_") and k.lower() not in {"fbclid", "gclid", "mc_cid", "mc_eid"}
        ])
        return urlunparse((p.scheme.lower(), host, path.rstrip("/") or "/", query, ""))
    except (TypeError, ValueError):
        return ""


def parse_date(value) -> date | None:
    """Parse Tavily or publisher dates without guessing missing dates."""
    if not value:
        return None
    if isinstance(value, date) and not isinstance(value, datetime):
        return value
    if isinstance(value, (int, float)):
        try:
            return datetime.fromtimestamp(value, timezone.utc).date()
        except (OverflowError, OSError, ValueError):
            return None
    raw = str(value).strip()
    if not raw:
        return None
    try:
        return date.fromisoformat(raw[:10])
    except ValueError:
        pass
    try:
        dt = datetime.fromisoformat(raw.replace("Z", "+00:00"))
        return dt.date()
    except ValueError:
        pass
    try:
        return parsedate_to_datetime(raw).date()
    except (TypeError, ValueError, OverflowError):
        return None


def freshness_status(published, today: date | None = None, days: int = FRESHNESS_DAYS) -> tuple[str, date | None]:
    today = today or datetime.now(timezone.utc).date()
    published_day = parse_date(published)
    if published_day is None:
        return "undated", None
    if published_day > today:
        return "future_date", published_day
    if published_day < today - timedelta(days=days):
        return "stale", published_day
    return "recent", published_day


def candidate_matches_database(candidate: dict, existing: list[dict], same_transaction) -> list[dict]:
    """Use production transaction identity logic, then exact canonical IDs as a fallback."""
    candidate_id = str(candidate.get("id") or "")
    matches = []
    for row in existing:
        if candidate_id and candidate_id == str(row.get("id") or ""):
            matches.append(row)
            continue
        try:
            if same_transaction(candidate, row):
                matches.append(row)
        except (KeyError, TypeError, ValueError):
            continue
    return matches


def classify_candidates(candidates: list[dict], existing: list[dict], same_transaction) -> list[dict]:
    out = []
    for candidate in candidates:
        matches = candidate_matches_database(candidate["deal"], existing, same_transaction)
        if matches:
            status = "already_in_database"
        elif candidate.get("freshness_status") == "recent" and candidate.get("source", {}).get("page_fetch_status") == "fetched":
            status = "potentially_new_needs_human_review"
        else:
            status = "not_eligible_as_new"
        candidate["database_match"] = [
            {"id": row.get("id"), "announced": row.get("announced"), "headline": row.get("headline"),
             "deal_type": row.get("deal_type"), "target": row.get("target")}
            for row in matches[:5]
        ]
        candidate["review_status"] = status
        out.append(candidate)
    return out


class ArticleParser(HTMLParser):
    """Extract article-focused paragraphs and publication metadata, not site-wide boilerplate."""
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.skip_depth = 0
        self.article_depth = 0
        self.main_depth = 0
        self.in_paragraph = False
        self.paragraph_parts = []
        self.paragraphs = []
        self.title_depth = 0
        self.title_parts = []
        self.meta = {}
        self._article_seen = False
        self._main_seen = False

    def handle_starttag(self, tag, attrs):
        a = {str(k).lower(): str(v) for k, v in attrs if v is not None}
        if tag in {"script", "style", "noscript", "svg", "nav", "footer", "header", "aside"}:
            self.skip_depth += 1
        if tag == "article":
            self.article_depth += 1
            self._article_seen = True
        if tag == "main":
            self.main_depth += 1
            self._main_seen = True
        if tag == "title":
            self.title_depth += 1
        if tag == "p":
            self.in_paragraph = True
            self.paragraph_parts = []
        if tag == "meta":
            key = (a.get("property") or a.get("name") or a.get("itemprop") or "").lower()
            val = a.get("content", "").strip()
            if key and val and key in {
                "article:published_time", "article:modified_time", "date", "datepublished",
                "pubdate", "publish-date", "parsely-pub-date", "og:title",
            }:
                self.meta.setdefault(key, val)

    def handle_endtag(self, tag):
        if tag in {"script", "style", "noscript", "svg", "nav", "footer", "header", "aside"} and self.skip_depth:
            self.skip_depth -= 1
        if tag == "article" and self.article_depth:
            self.article_depth -= 1
        if tag == "main" and self.main_depth:
            self.main_depth -= 1
        if tag == "title" and self.title_depth:
            self.title_depth -= 1
        if tag == "p" and self.in_paragraph:
            value = re.sub(r"\s+", " ", " ".join(self.paragraph_parts)).strip()
            if value:
                self.paragraphs.append(value)
            self.paragraph_parts = []
            self.in_paragraph = False

    def handle_data(self, data):
        if self.skip_depth:
            return
        value = re.sub(r"\s+", " ", data).strip()
        if not value:
            return
        if self.title_depth:
            self.title_parts.append(value)
        if self.in_paragraph:
            self.paragraph_parts.append(value)

    def result(self):
        paragraphs = self.paragraphs
        # Prefer <article> / <main> extraction when present; fallback remains paragraph-only.
        title = re.sub(r"\s+", " ", " ".join(self.title_parts)).strip()
        return title[:300], paragraphs[:120], self.meta


def parse_article(markup: str) -> dict:
    parser = ArticleParser()
    try:
        parser.feed(markup[:2_000_000])
    except Exception:
        pass
    title, paragraphs, meta = parser.result()
    deal_terms = re.compile(
        r"\b(raises?|raised|funding|investment|invests?|acquires?|acquisition|merger|merges?|"
        r"private equity|venture capital|fund close|first close|final close|loan|facility|financing|"
        r"levée|lève|rachat|acquiert|investissement|financement|aquisição|financiamento|investimento)\b",
        re.IGNORECASE,
    )
    relevant = [p for p in paragraphs if deal_terms.search(p)]
    # Use a small lead + transaction-specific paragraphs. Never feed the entire page to the extractor.
    chosen = list(dict.fromkeys((paragraphs[:2] + relevant[:5])))
    lead = re.sub(r"\s+", " ", " ".join(chosen)).strip()[:3500]
    pub_date = next((meta[k] for k in (
        "article:published_time", "datepublished", "parsely-pub-date", "publish-date", "pubdate", "date"
    ) if meta.get(k)), None)
    return {"title": title, "lead": lead, "published_date": pub_date, "paragraph_count": len(paragraphs)}


def fetch_database(api_base: str, client: httpx.Client, page_size: int = 100) -> list[dict]:
    """Fetch every current visible deal through the public read-only API; fail closed on partial data."""
    all_rows = []
    expected_total = None
    offset = 0
    while expected_total is None or offset < expected_total:
        response = client.get(
            f"{api_base.rstrip('/')}/api/deals",
            params={"limit": page_size, "offset": offset},
            headers={"Accept": "application/json", "User-Agent": USER_AGENT},
        )
        response.raise_for_status()
        payload = response.json()
        rows = payload.get("deals")
        total = payload.get("total_in_database")
        if not isinstance(rows, list) or not isinstance(total, int) or total < 0:
            raise RuntimeError("Production deals API returned an unexpected schema; refusing to continue.")
        if expected_total is None:
            expected_total = total
            if expected_total == 0:
                raise RuntimeError("Production deals API returned zero records; refusing to treat an empty snapshot as complete.")
        elif total != expected_total:
            raise RuntimeError("Production deal count changed during pagination; rerun to avoid partial deduplication.")
        if not rows and offset < expected_total:
            raise RuntimeError(f"Production deals API returned an empty page at offset {offset}.")
        all_rows.extend(rows)
        offset += len(rows)
        if len(rows) < page_size and offset < expected_total:
            raise RuntimeError(f"Production deals API stopped at {offset}/{expected_total}; refusing partial deduplication.")
    if expected_total is not None and len(all_rows) != expected_total:
        raise RuntimeError(f"Loaded {len(all_rows)} of {expected_total} production deals; refusing partial deduplication.")
    return all_rows


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--deal-intel-root", required=True)
    ap.add_argument("--output", default="deal-intelligence-discovery.json")
    ap.add_argument("--max-pages", type=int, choices=(5, 10, 15), default=10)
    ap.add_argument("--freshness-days", type=int, choices=(7, 14, 30), default=30)
    ap.add_argument("--api-base-url", default=os.environ.get("DEAL_INTEL_API_BASE_URL", API_DEFAULT))
    args = ap.parse_args()
    key = os.environ.get("TAVILY_API_KEY", "").strip()
    if not key:
        raise SystemExit("TAVILY_API_KEY Actions secret is missing.")
    api_base = args.api_base_url.rstrip("/")
    parsed_api = urlparse(api_base)
    if parsed_api.scheme != "https" or not parsed_api.hostname:
        raise SystemExit("DEAL_INTEL_API_BASE_URL must be a valid HTTPS base URL.")

    sys.path.insert(0, str(Path(args.deal_intel_root).resolve()))
    try:
        from src.engines.deals import extract_deal, same_transaction
    except Exception as exc:
        raise SystemExit(f"Cannot import Deal Intelligence extractor: {type(exc).__name__}") from exc

    proxy_url = os.environ.get("HARVESTER_PROXY_URL", "").strip()
    start = time.monotonic()
    today = datetime.now(timezone.utc).date()
    since = today - timedelta(days=args.freshness_days)
    queries = [f"{q} {today.year}" for q in QUERY_TEMPLATES]
    by_url, query_stats, fetch_errors = {}, [], {}

    # Search and production API reads are direct; only publisher-page requests use the proxy.
    with httpx.Client(timeout=PAGE_TIMEOUT, follow_redirects=True) as client:
        for query in queries:
            try:
                response = client.post(
                    "https://api.tavily.com/search",
                    headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"},
                    json={"query": query, "topic": "news", "time_range": "month", "days": args.freshness_days,
                          "search_depth": "advanced", "max_results": MAX_RESULTS_PER_QUERY,
                          "include_answer": False, "include_raw_content": False},
                )
                response.raise_for_status()
                rows = response.json().get("results") or []
                query_stats.append({"query": query, "status": "ok", "results": len(rows)})
                for row in rows:
                    url = normalize_url(str(row.get("url") or ""))
                    if not url:
                        continue
                    if url not in by_url:
                        by_url[url] = {
                            "title": str(row.get("title") or "")[:500], "url": url,
                            "publisher": (urlparse(url).hostname or "").lower(),
                            "published_date": row.get("published_date"),
                            "search_snippet": str(row.get("content") or "")[:1500],
                            "matched_queries": [query],
                        }
                    elif query not in by_url[url]["matched_queries"]:
                        by_url[url]["matched_queries"].append(query)
            except Exception as exc:
                query_stats.append({"query": query, "status": "error", "error_type": type(exc).__name__})

        if not any(q["status"] == "ok" for q in query_stats):
            raise SystemExit("All Tavily searches failed; no discovery results were generated.")

        # Load all production records before classifying anything as missing.
        try:
            database = fetch_database(api_base, client)
        except Exception as exc:
            raise SystemExit(f"Could not load the complete production deal database ({type(exc).__name__}); refusing to label candidates as new.") from exc

    results = list(by_url.values())
    for item in results:
        status, parsed = freshness_status(item.get("published_date"), today=today, days=args.freshness_days)
        item["freshness_status"] = status
        item["published_day"] = parsed.isoformat() if parsed else None
        item["freshness_cutoff"] = since.isoformat()
        item["page_fetch_status"] = "not_selected"

    # Process newest dated results first; do not let old/undated hits consume the fetch budget.
    recent_results = [r for r in results if r["freshness_status"] == "recent"]
    recent_results.sort(key=lambda r: (r["published_day"] or "", r["url"]), reverse=True)
    fetch_opts = {"timeout": PAGE_TIMEOUT, "follow_redirects": True}
    if proxy_url:
        fetch_opts["proxy"] = proxy_url
    candidates = []
    with httpx.Client(**fetch_opts, headers={"User-Agent": USER_AGENT, "Accept": "text/html,application/xhtml+xml"}) as client:
        for item in recent_results[:args.max_pages]:
            item["page_fetch_status"] = "fetch_failed"
            try:
                response = client.get(item["url"])
                item["http_status"] = response.status_code
                content_type = response.headers.get("content-type", "").lower()
                if not response.is_success or "text/html" not in content_type:
                    fetch_errors[f"http_{response.status_code}"] = fetch_errors.get(f"http_{response.status_code}", 0) + 1
                    continue
                article = parse_article(response.text)
                if article["title"]:
                    item["page_title"] = article["title"]
                item["page_fetch_status"] = "fetched"
                item["article_paragraph_count"] = article["paragraph_count"]
                fetched_day = parse_date(article.get("published_date"))
                # Publisher metadata outranks search metadata, but must still pass the freshness gate.
                if fetched_day:
                    item["publisher_published_day"] = fetched_day.isoformat()
                    item["published_day"] = fetched_day.isoformat()
                    item["freshness_status"] = freshness_status(fetched_day, today=today, days=args.freshness_days)[0]
                title = item.get("page_title") or item["title"]
                lead = article.get("lead") or ""
                # Avoid page-wide sidebar/related-story contamination that caused bad sector labels.
                evidence = "\n".join(part for part in (title, item.get("search_snippet", ""), lead) if part)
                item["page_evidence_excerpt"] = evidence[:1600]
                if item["freshness_status"] != "recent":
                    item["candidate_status"] = "skipped_not_recent_after_publisher_date_check"
                    continue
                deal = extract_deal(
                    title, "\n".join(part for part in (item.get("search_snippet", ""), lead[:2200]) if part),
                    published=item["published_day"], publisher=item["publisher"], url=item["url"], africa_source=True,
                )
                if not deal:
                    item["candidate_status"] = "no_deal_extracted"
                    continue
                # Sector must be supported by the headline or article lead, not unrelated page chrome.
                from src.engines.deals import classify_sector
                focused_sector = classify_sector(" ".join((title, lead[:900])))
                if focused_sector:
                    deal["sector"] = focused_sector
                candidates.append({
                    "deal": deal, "freshness_status": item["freshness_status"],
                    "source": {"title": title[:500], "url": item["url"], "publisher": item["publisher"],
                               "published_date": item["published_day"], "matched_queries": item["matched_queries"],
                               "page_fetch_status": item["page_fetch_status"],
                               "evidence_excerpt": item.get("page_evidence_excerpt", "")[:1200]},
                    "review_status": "candidate_pending_database_deduplication",
                })
            except Exception as exc:
                kind = type(exc).__name__
                fetch_errors[kind] = fetch_errors.get(kind, 0) + 1
                item["page_fetch_status"] = f"error_{kind}"

    candidates = classify_candidates(candidates, database, same_transaction)
    summary = {
        "queries_succeeded": sum(q["status"] == "ok" for q in query_stats),
        "queries_failed": sum(q["status"] != "ok" for q in query_stats),
        "unique_urls_discovered": len(results),
        "recent_dated_urls": len(recent_results),
        "source_pages_attempted": min(len(recent_results), args.max_pages),
        "source_pages_fetched": sum(r["page_fetch_status"] == "fetched" for r in recent_results[:args.max_pages]),
        "fetch_errors_by_type": fetch_errors,
        "production_database_records_loaded": len(database),
        "deal_candidates_extracted": len(candidates),
        "matched_existing_database": sum(c["review_status"] == "already_in_database" for c in candidates),
        "potentially_new_for_human_review": sum(c["review_status"] == "potentially_new_needs_human_review" for c in candidates),
        "not_eligible_as_new": sum(c["review_status"] == "not_eligible_as_new" for c in candidates),
        "elapsed_seconds": round(time.monotonic() - start, 2),
    }
    # Only source pages fetched successfully and confirmed recent are eligible for candidate output.
    output = {
        "schema_version": 2, "run_at_utc": datetime.now(timezone.utc).isoformat(),
        "mode": "review_only_no_production_writes",
        "freshness_policy": {"days": args.freshness_days, "cutoff": since.isoformat(),
                             "only_recent_publisher_pages_can_be_labelled_potentially_new": True},
        "database_comparison": {"api_base_url": api_base, "records_loaded": len(database),
                                "complete_snapshot_required": True, "writes_performed": False},
        "limits": {"queries": len(queries), "max_results_per_query": MAX_RESULTS_PER_QUERY,
                   "max_source_pages_fetched": args.max_pages, "proxy_configured": bool(proxy_url),
                   "proxy_credentials_written": False, "raw_html_written": False},
        "summary": summary, "queries": query_stats, "results": results, "deal_candidates": candidates,
        "limitations": [
            "A potentially-new label means no match was found in the complete current database; it is not analyst-verified.",
            "The pilot does not write to production. Human review is required before ingestion.",
            "Only dated recent publisher pages can be labelled potentially new.",
            "This bounded discovery run is not comprehensive Bloomberg-equivalent coverage.",
        ],
    }
    out = Path(args.output)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(output, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"summary": summary, "proxy_configured": bool(proxy_url), "output": str(out)}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
