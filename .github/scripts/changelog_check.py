#!/usr/bin/env python3
"""Require pull requests to update the project changelog."""

from __future__ import annotations

import argparse
import os
import subprocess
import sys
from pathlib import Path


CHANGELOG = "CHANGELOG.md"


def run_git(args: list[str], repo: Path) -> str:
    completed = subprocess.run(
        ["git", *args],
        cwd=repo,
        check=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    return completed.stdout


def changed_files(repo: Path, base_ref: str) -> set[str]:
    candidates = [f"origin/{base_ref}...HEAD", f"{base_ref}...HEAD"]
    last_error = ""
    for revision_range in candidates:
        try:
            output = run_git(["diff", "--name-only", revision_range], repo)
            return {line.strip() for line in output.splitlines() if line.strip()}
        except subprocess.CalledProcessError as exc:
            last_error = exc.stderr.strip()
    raise RuntimeError(f"could not compare against {base_ref}: {last_error}")


def validate_changelog(repo: Path) -> list[str]:
    errors: list[str] = []
    changelog = repo / CHANGELOG
    if not changelog.exists():
        errors.append(f"{CHANGELOG} is missing")
        return errors

    text = changelog.read_text(encoding="utf-8", errors="ignore")
    if not text.startswith("# Changelog"):
        errors.append(f"{CHANGELOG} must start with '# Changelog'")
    if "## Unreleased" not in text:
        errors.append(f"{CHANGELOG} must include a '## Unreleased' section")
    return errors


def check(repo: Path, base_ref: str | None, event_name: str | None) -> int:
    errors = validate_changelog(repo)
    if errors:
        for error in errors:
            print(f"ERROR: {error}", file=sys.stderr)
        return 1

    if event_name != "pull_request":
        print("Changelog structure is valid; update requirement only applies to pull requests.")
        return 0

    if not base_ref:
        print("ERROR: base ref is required for pull request changelog validation", file=sys.stderr)
        return 1

    changed = changed_files(repo, base_ref)
    if CHANGELOG not in changed:
        print(
            f"ERROR: pull requests must update {CHANGELOG}. "
            "Add an entry under '## Unreleased' describing the user-visible change.",
            file=sys.stderr,
        )
        return 1

    print(f"{CHANGELOG} was updated in this pull request.")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("repo", nargs="?", default=".", help="repository path")
    parser.add_argument("--base-ref", default=os.environ.get("GITHUB_BASE_REF"), help="base branch for PR comparison")
    parser.add_argument("--event-name", default=os.environ.get("GITHUB_EVENT_NAME"), help="GitHub event name")
    args = parser.parse_args()

    try:
        return check(Path(args.repo).resolve(), args.base_ref, args.event_name)
    except RuntimeError as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
