# Security policy

tiny-bank is a demonstration banking platform. It holds no real money and no real customer data, and runs only on a local Kubernetes cluster. Its code is still written to the standard a bank would expect, so security reports are welcome.

## Reporting a vulnerability

Please report it privately, through GitHub: open the repository's **Security** tab and choose **Report a vulnerability**. Don't open a public issue or pull request, and don't describe the problem in a commit message.

A useful report says:

- what the weakness is, and which file, endpoint or setting it's in;
- how to reproduce it, ideally with a `curl` command or a failing test;
- what an attacker could do with it.

Reports are handled on a best-effort basis, usually within a week. You'll get a reply saying whether the report is accepted, and the fix will credit you unless you'd rather it didn't.

## What's in scope

Everything in this repository: the services, the shared code in `pkg/`, the contracts in `proto/`, the Helm chart and cluster settings in `deploy/`, the image recipe and the CI workflow. Only the `main` branch is supported.

Some weaknesses are already known and planned for, such as the API having no sign-in until the edge gateway arrives. They're listed under "Known gaps" in [docs/security.md](docs/security.md). A report that shows one of them is worse than described there is still welcome.

## Out of scope

- Settings that exist only for local development and are marked that way, such as Grafana allowing anyone to log in as admin. Grafana is only reachable through `kubectl port-forward` on localhost.
- Problems in third-party software, unless tiny-bank uses it in an unsafe way. Report those to the project itself.
- Denial of service by sending large amounts of traffic to a local cluster.
