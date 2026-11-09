#!/usr/bin/env python3
"""Keep the newest N builds of each GHCR package and delete the rest.

A "build" is a top-level manifest plus every child manifest it references.
buildx publishes an index and its platform/attestation manifests as separate
package versions; a child must never outlive its parent, so children are only
deleted together with the index that owns them.

Never deleted:
  * floating tags (latest, main, stable, prod, release)
  * semantic-version tags (v1.2.3, 0.1.0)
  * any build whose tag or digest is listed in --protected (deployed images)
  * the newest --keep builds

Everything else, including per-build tags such as a 40-char SHA, sha-abc123,
git-abc123 or main-abc123, is prunable once it falls outside the keep window.
"""

import argparse
import base64
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor

API = "https://api.github.com"
REGISTRY = "https://ghcr.io"
ACCEPT = ", ".join([
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.docker.distribution.manifest.v2+json",
])
FLOATING = re.compile(r"^(latest|main|master|stable|prod|release)$", re.I)
SEMVER = re.compile(r"^v?\d+\.\d+\.\d+$")


def request(url, token, method="GET", data=None, accept=ACCEPT):
    req = urllib.request.Request(url, method=method, data=data)
    req.add_header("Authorization", f"Bearer {token}")
    req.add_header("Accept", accept)
    req.add_header("X-GitHub-Api-Version", "2022-11-28")
    return urllib.request.urlopen(req, timeout=60)


def api(path, method="GET"):
    with request(API + path, TOKEN, method, accept="application/vnd.github+json") as r:
        body = r.read()
        return json.loads(body) if body else None


def list_versions(org, package):
    out, page = [], 1
    enc = urllib.parse.quote(package, safe="")
    while True:
        chunk = api(f"/orgs/{org}/packages/container/{enc}/versions?per_page=100&page={page}")
        if not chunk:
            break
        out.extend(chunk)
        if len(chunk) < 100:
            break
        page += 1
    return out


def registry_token(org, package):
    url = f"{REGISTRY}/token?scope=repository:{org}/{package}:pull&service=ghcr.io"
    req = urllib.request.Request(url)
    creds = base64.b64encode(f"{ACTOR}:{TOKEN}".encode()).decode()
    req.add_header("Authorization", "Basic " + creds)
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)["token"]


def manifest(org, package, digest, token):
    try:
        with request(f"{REGISTRY}/v2/{org}/{package}/manifests/{digest}", token) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        if e.code == 404:
            return None
        raise


def delete_version(org, package, version_id, dry_run):
    if dry_run:
        return True, None
    enc = urllib.parse.quote(package, safe="")
    path = f"/orgs/{org}/packages/container/{enc}/versions/{version_id}"
    for attempt in range(5):
        try:
            with request(API + path, TOKEN, method="DELETE", accept="application/vnd.github+json"):
                return True, None
        except urllib.error.HTTPError as e:
            if e.code in (429, 500, 502, 503, 504):
                time.sleep(2 ** attempt)
                continue
            return False, f"HTTP {e.code}: {e.read().decode()[:200]}"
        except urllib.error.URLError:
            time.sleep(2 ** attempt)
    return False, "retries exhausted"


def protected_ref(ref):
    bare = ref.removeprefix("sha256:")
    return ref in PROTECTED or bare in PROTECTED or f"sha256:{bare}" in PROTECTED


def tag_protected(tag):
    return bool(FLOATING.match(tag) or SEMVER.match(tag))


def prune(package, keep, dry_run):
    versions = list_versions(ORG, package)
    if not versions:
        print(f"{package}: no versions, nothing to do")
        return 0, 0

    token = registry_token(ORG, package)
    with ThreadPoolExecutor(max_workers=6) as ex:
        manifests = list(ex.map(lambda v: manifest(ORG, package, v["name"], token), versions))

    by_digest = {v["name"]: v for v in versions}
    children = {}
    for v, m in zip(versions, manifests):
        children[v["name"]] = {c["digest"] for c in m["manifests"]} if m and "manifests" in m else set()

    all_children = set()
    for kids in children.values():
        all_children |= kids
    top_level = [v for v in versions if v["name"] not in all_children]
    if not top_level:
        print(f"{package}: WARNING every version is a child manifest; refusing to prune")
        return 0, 0

    groups = []
    for v in top_level:
        members = [v["name"]] + sorted(c for c in children[v["name"]] if c in by_digest)
        groups.append({
            "digest": v["name"],
            "created": v["created_at"],
            "versions": members,
            "tags": v["metadata"]["container"].get("tags") or [],
            "member_tags": [t for m in members for t in (by_digest[m]["metadata"]["container"].get("tags") or [])],
        })
    groups.sort(key=lambda g: g["created"], reverse=True)

    kept_names = set()
    for i, g in enumerate(groups):
        reasons = []
        if i < keep:
            reasons.append("newest")
        for tag in g["member_tags"]:
            if tag_protected(tag):
                reasons.append(f"tag:{tag}")
        if protected_ref(g["digest"]) or any(protected_ref(t) for t in g["tags"]):
            reasons.append("deployed")
        if any(protected_ref(c) for c in children[g["digest"]]):
            reasons.append("child-deployed")
        if reasons:
            kept_names.update(g["versions"])
            print(f"  keep {g['created'][:10]} {','.join(g['tags']) or '<untagged>'} ({', '.join(reasons)})")

    delete = []
    for g in groups:
        if g["digest"] in kept_names:
            continue
        for name in g["versions"]:
            if name in kept_names:
                continue
            v = by_digest[name]
            delete.append((v["created_at"], v))
    delete.sort(key=lambda x: x[0])

    deleted = failed = 0
    for created, v in delete:
        ok, err = delete_version(ORG, package, v["id"], dry_run)
        tags = v["metadata"]["container"].get("tags") or []
        label = f"{created[:10]} {','.join(tags) or '<untagged>'} {v['name'][:19]}"
        if ok:
            deleted += 1
            print(f"  {'would delete' if dry_run else 'deleted'} {label}")
        else:
            failed += 1
            print(f"  FAILED {label}: {err}")

    print(f"{package}: {len(versions)} versions, {len(groups)} builds, "
          f"kept {len(kept_names)} versions, {'would delete' if dry_run else 'deleted'} {deleted}, failed {failed}")
    return deleted, failed


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--keep", type=int, default=int(os.environ.get("KEEP", "2")))
    parser.add_argument("--packages", nargs="+",
                        default=os.environ.get("PACKAGES", "").replace(",", " ").split())
    parser.add_argument("--protected", nargs="*", default=[])
    parser.add_argument("--dry-run", action="store_true",
                        default=os.environ.get("DRY_RUN", "").lower() == "true")
    args = parser.parse_args()
    PROTECTED.update(args.protected)
    if not args.packages:
        sys.exit("no packages configured")

    total_deleted = total_failed = 0
    for package in args.packages:
        print(f"== {package}")
        d, f = prune(package, args.keep, args.dry_run)
        total_deleted += d
        total_failed += f
    print(f"total: {'would delete' if args.dry_run else 'deleted'} {total_deleted}, failed {total_failed}")
    if total_failed:
        sys.exit(1)


if __name__ == "__main__":
    TOKEN = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
    ACTOR = os.environ.get("GHCR_ACTOR") or os.environ.get("GITHUB_ACTOR")
    ORG = os.environ.get("GITHUB_ORG") or os.environ.get("GITHUB_REPOSITORY_OWNER")
    PROTECTED = set(os.environ.get("PROTECTED_DIGESTS", "").split())
    if not (TOKEN and ACTOR and ORG):
        sys.exit("GH_TOKEN, GHCR_ACTOR and GITHUB_ORG are required")
    main()
