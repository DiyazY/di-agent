#!/usr/bin/env python3
"""Download all CSV files from the shared SharePoint folder into poc2/data/.

The share URL is read from DATA_URL (environment or the repo-root .env file).
Usage: python poc2/download_data.py [--force]
"""
import http.cookiejar
import json
import os
import sys
import urllib.parse
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DEST = Path(__file__).resolve().parent / "data"


def load_env():
    env_file = ROOT / ".env"
    if env_file.exists():
        for line in env_file.read_text().splitlines():
            line = line.strip()
            if line and not line.startswith("#") and "=" in line:
                k, v = line.split("=", 1)
                os.environ.setdefault(k.strip(), v.strip().strip("\"'"))


def main():
    load_env()
    url = os.environ.get("DATA_URL")
    if not url:
        sys.exit("DATA_URL is not set (put it in .env)")
    force = "--force" in sys.argv

    opener = urllib.request.build_opener(
        urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
    # Following the share link redirects to the folder and sets an anonymous session cookie.
    final = opener.open(url).geturl()
    parsed = urllib.parse.urlparse(final)
    folder = urllib.parse.parse_qs(parsed.query)["id"][0]
    site = "/".join(folder.split("/")[:3])  # /personal/<user>
    base = f"{parsed.scheme}://{parsed.netloc}{site}/_api/web"
    folder_q = urllib.parse.quote(folder, safe="")

    req = urllib.request.Request(
        f"{base}/GetFolderByServerRelativeUrl('{folder_q}')/Files?$select=Name,ServerRelativeUrl,Length",
        headers={"Accept": "application/json;odata=nometadata"})
    files = json.load(opener.open(req))["value"]
    csvs = [f for f in files if f["Name"].lower().endswith(".csv")]

    DEST.mkdir(parents=True, exist_ok=True)
    for f in csvs:
        target = DEST / f["Name"]
        size = int(f["Length"])
        if not force and target.exists() and target.stat().st_size == size:
            print(f"skip   {f['Name']}")
            continue
        print(f"get    {f['Name']} ({size / 1e6:.1f} MB)")
        rel = urllib.parse.quote(f["ServerRelativeUrl"], safe="")
        with opener.open(f"{base}/GetFileByServerRelativeUrl('{rel}')/$value") as r, \
                open(target.with_suffix(".part"), "wb") as out:
            while chunk := r.read(1 << 20):
                out.write(chunk)
        target.with_suffix(".part").rename(target)
    print(f"Done: {len(csvs)} CSV file(s) in {DEST}")


if __name__ == "__main__":
    main()
