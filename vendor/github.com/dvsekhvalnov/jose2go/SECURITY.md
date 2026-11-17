# Security Policy

## Reporting a Vulnerability

The security of this project and the users who depend on it is important.

If you believe you have found a security vulnerability, please report it through **GitHub Private Vulnerability Reporting**.

Please do **not** open a public GitHub issue for security vulnerabilities before coordinated disclosure. Public reports may expose users of this library to unnecessary risk.

Additional contact information is available on the project's GitHub About page.

---

## Scope

Security reports are welcome for the entire repository.

Any type of potential security vulnerability is welcome, including but not limited to:

* Code execution vulnerabilities
* Authentication or authorization issues
* Data exposure
* Injection vulnerabilities
* Denial-of-service vulnerabilities
* Cryptographic issues
* Supply-chain or dependency-related security issues


Dependency-related reports are welcome when they have a demonstrated security impact on this library or its users. Please include evidence of possible exploitation or security impact.

---

## Supported Versions

Security fixes are provided for the latest released version only.

Users of older versions are encouraged to upgrade to the latest release before requesting security fixes.

Reports affecting older versions are still welcome if they help identify security issues, but fixes will be developed against the latest supported version.

---

## What to Include in a Report

To help with investigation and resolution, please include as much of the following information as possible:

* A clear description of the vulnerability
* Expected behavior and actual behavior
* A minimal reproducible example in a form of unit test demonstrating exploitation
* Impact assessment, if possible
* Severity assessment, if possible

If AI-assisted tools were used during security research, please mention this in the report and describe how they contributed to the investigation. This information helps with effective triage and evaluation of the findings.

---

## Response Timeline

The project aims to follow these response timelines:

* **Acknowledgement:** within 1–5 business days (excluding holidays)
* **Initial assessment:** within 7 days
* **Fix timeline:** depends on vulnerability complexity, impact, and required changes

Security reports are investigated carefully, and timelines may vary depending on the nature of the issue.

---

## Vulnerability Handling Process

Security fixes follow the normal GitHub development workflow:

1. A fix is developed in a dedicated branch.
2. Changes may be contributed by either the project maintainer or the security researcher.
3. The fix is reviewed and merged through the normal pull request process.
4. After the fix is merged into `master` and tagged/released, coordinated public disclosure can take place.

---

## Disclosure Policy

The project follows a standard coordinated disclosure process.

Security details should remain private until a fix has been merged into `master` and a corresponding release/tag is available.

After the fix is available:

* The security researcher may publish the details or open a public GitHub issue.
* The project maintainer may publish the disclosure on behalf of the researcher if requested.
* Researcher credit will always be preserved.
* A specific disclosure date may be requested and negotiated when additional coordination is needed.

---

## Researcher Recognition

Security researchers who responsibly report vulnerabilities are credited in the corresponding GitHub Security Advisory.

Additional acknowledgement may also be provided through appropriate project pages or GitHub discussions/issues when applicable.

Thank you for helping improve the security of this project and the wider open-source ecosystem.

---

## Safe Harbor

The project welcomes security research performed in good faith.

If you:

* Follow responsible disclosure practices
* Avoid intentionally harming users or services
* Avoid accessing or modifying data that does not belong to you
* Provide reasonable details needed to reproduce and fix the issue
* Allow reasonable time for remediation and coordinated disclosure

then the project will consider your research to be authorized security testing and will not pursue legal action against you for your security research activities.

If you are unsure whether a specific action is acceptable, please contact the project maintainers before proceeding.
