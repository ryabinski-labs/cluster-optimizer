# Security Policy

## Supported Versions

Security fixes are made on the default branch. Tagged releases are supported when they are the latest published release.

## Reporting a Vulnerability

Report vulnerabilities privately. Use GitHub private vulnerability reporting if it is enabled for this repository. If that is unavailable, contact `cigan1@gmail.com`.

Do not include secrets, kubeconfigs, access keys, customer data, production logs, or exploit details in public issues or pull requests. If you find a secret-like value, report the path and key name without repeating the value.

## Scope

In scope:

- The Cluster Optimizer Go binaries and static UI.
- Kubernetes manifests and example deployment files.
- GitHub Actions workflows and CloudFormation templates in this repository.

Out of scope:

- Clusters, cloud accounts, or application repositories operated by users.
- Workloads that Cluster Optimizer inspects but does not own.
- Social engineering, physical attacks, or denial-of-service testing.

## Disclosure

The maintainer will review reports on a best-effort basis, confirm impact, and coordinate a fix before public disclosure. This repository does not provide a guaranteed response-time service level.
