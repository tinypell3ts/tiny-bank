# proto/AGENTS.md

Rules for the protobuf contracts under `proto/`. The root [AGENTS.md](../AGENTS.md) still applies. Why services talk protobuf over Typhon is in [ADR-0007](../docs/adr/0007-build-services-on-typhon-and-terrors.md).

A contract is a promise to every caller, including ones deployed before your change. A service can be rolled back or updated in any order, so the contract must keep working both ways.

## Layout

```
proto/
├── buf.yaml                  # the buf module: lint (STANDARD) and breaking (FILE) rules
├── buf.gen.yaml              # generates Go next to each .proto, with the protoc-gen-go pinned in go.mod
└── <service>/v<N>/           # one versioned package per service API, such as account/v1
    ├── <service>.proto       # the contract
    └── <service>.pb.go       # generated: never edit, always commit
```

## Rules

- **Run buf through make.** `make proto` regenerates the Go code, and `make proto-check` checks formatting, lint, breaking changes against `main`, and that the committed Go code matches. `make check` and CI run `proto-check`.
- **Commit the `.proto` and its generated `.pb.go` together.** Never edit a `.pb.go` file: change the `.proto` and run `make proto`.
- **Never break a published version.** Once a package such as `account.v1` is on `main`, don't renumber, rename or retype a field, or reuse a number. A change that has to break things goes in a new package (`account.v2`), served alongside the old one until no caller uses it.
- **Field names are part of the JSON API.** Services send JSON using the proto field names (`customer_id`), so renaming a field breaks JSON callers even though binary protobuf wouldn't notice. buf's FILE rules catch it.
- **Strict requests decide the order of a change.** A service reads requests with `kit.Decode`, strictly: a JSON field or enum value it doesn't know is a 400, so a typo like `currancy` is never silently dropped. (Binary request bodies arrive in Phase 8, and will be just as strict: a field the service doesn't know is a 400, not silently kept.) So an "additive" change is only safe in the right order:
  - **New request field:** deploy the service that reads it first, then let callers send it. A caller that sends it to an older service gets a 400.
  - **Retiring a request field:** first stop every caller sending it, then delete it and `reserve` its number and name, so neither is reused by mistake. Until then, mark it `[deprecated = true]` and keep accepting it.
  - **New response field or enum value:** safe only because callers read responses leniently, ignoring fields and enum values they don't know (`kit.Get` reads binary protobuf, which skips unknown fields and keeps an unknown enum value as a bare number). A strict reader would fail on the first response that has one.

  buf can't check this order: it compares files, not deployments. Say in the PR which service must ship first.
- **Names follow buf's STANDARD lint.** Fields are `lower_snake_case`. Enum values are prefixed with the enum's name, and the zero value is `<ENUM>_UNSPECIFIED`, which always means "not set". New enum values go at the end. A caller reading leniently gets a value it doesn't know as the zero value (JSON) or as a bare number (binary), and must treat it as the safe choice: for `AccountStatus`, "not open".
- **The comments are the documentation.** Every message and field gets a comment saying what it means and what values are allowed. The service enforces those rules; the contract describes them.
- **Proto types stay at the edge.** Handlers convert between proto messages and the service's own types. The domain and the store never import generated code, so the wire format can change without touching business logic.
- **Money is never a float.** An amount is an `int64` in minor units plus an ISO 4217 currency code, as in `pkg/money` ([ADR-0004](../docs/adr/0004-represent-money-as-int64-minor-units.md)). Never use `float` or `double` for money.
- **Bump the tools on purpose.** buf's version is in CI (`go install …buf@vX`); keep it in step with your local `buf --version`. protoc-gen-go is a Go tool in `go.mod`: change it with `go get -tool google.golang.org/protobuf/cmd/protoc-gen-go@vX`, then run `make proto` and commit the regenerated code.
