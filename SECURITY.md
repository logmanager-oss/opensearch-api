# Security policy

## Supported versions

Only the latest minor release line receives security fixes. A fix ships as a new patch
release of that line. Older minors are not patched; upgrade to the latest release.

## Reporting a vulnerability

Report vulnerabilities privately through GitHub:
<https://github.com/logmanager-oss/opensearch-api/security/advisories/new>.
Do not open a public issue for a suspected vulnerability.

Include the affected version, the steps to reproduce, and the impact you see.

## Response

- We acknowledge a report within 7 days.
- We aim to release a fix, or to agree a disclosure date with you, within 90 days of the
  report. Coordinated disclosure happens through a GitHub security advisory, which credits the
  reporter unless they ask otherwise.

## Release integrity

Every release is built by GitHub Actions. The archives and the checksums file are signed with
cosign (keyless, Sigstore) and carry a SLSA provenance attestation. Verify a download with the
commands under "Verify a release download" in [README.md](README.md). Both commands pin the
builder workflow. A check with `--repo` alone accepts an attestation from any workflow in the
repository.
