# Changelog

All notable changes to Cluster Optimizer will be documented in this file.

## Unreleased

- Unified both rewrite and resource-remediation actions to display a beautifully-styled, local-first instructions preview modal with clipboard-copy and markdown-download capabilities.
- Implemented direct browser markdown file downloads for rewrite modernization plans.
- Replaced standard browser confirm popups with a beautiful, modern native in-app confirmation modal.
- Fixed UI observed-day calculation by implementing paginated DynamoDB queries and stateful recommendation tracking to prevent sliding-window resets.
- Fixed UI observed-day counts to advance on UTC calendar-day boundaries instead of waiting for a full 24 hours.
- Updated Kubernetes patch dependencies while staying on the supported 0.35 module line.
- Updated GitHub Actions workflow dependencies.
- Stabilized Dependabot policy for supported Kubernetes module versions and generated files.
- Added pull request validation to require `CHANGELOG.md` updates on every PR.
- Prepared repository controls, documentation, and workflows for public open-source release.
