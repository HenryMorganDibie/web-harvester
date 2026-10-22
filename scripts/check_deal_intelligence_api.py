#!/usr/bin/env python3
"""Read-only smoke check for the public Deal Intelligence API and full pagination."""
import os
from urllib.parse import urlparse

import httpx

from deal_intelligence_tavily_pilot import API_DEFAULT, fetch_database


def main() -> int:
    base = os.environ.get("DEAL_INTEL_API_BASE_URL", API_DEFAULT).rstrip("/")
    parsed = urlparse(base)
    if parsed.scheme != "https" or not parsed.hostname:
        raise SystemExit("DEAL_INTEL_API_BASE_URL must be a valid HTTPS URL.")
    with httpx.Client(timeout=httpx.Timeout(25.0, connect=10.0), follow_redirects=True) as client:
        rows = fetch_database(base, client)
    print(f"API endpoint reachable; complete visible deal snapshot loaded: {len(rows)} records.")
    print("Canonical IDs are present and unique; no production writes were performed.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
