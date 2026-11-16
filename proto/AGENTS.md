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
- **Never break a published version.** Once a package such as `account.v1` is on `main`, don't renumber, remove, rename or retype a field, or remove an enum value. To retire a field, delete it and `reserve` both its number and its name, so neither can be reused by mistake. A change that has to break things goes in a new package (`account.v2`), served alongside the old one until no caller uses it.
- **Field names are part of the JSON API.** Services send JSON using the proto field names (`customer_id`), so renaming a field breaks JSON callers even though binary protobuf wouldn't notice. buf's FILE rules catch it.
- **Names follow buf's STANDARD lint.** Fields are `lower_snake_case`. Enum values are prefixed with the enum's name, and the zero value is `<ENUM>_UNSPECIFIED`, which always means "not set". New enum values go at the end, and readers treat a value they don't know as the safe choice (for `AccountStatus`, "not open").
- **The comments are the documentation.** Every message and field gets a comment saying what it means and what values are allowed. The service enforces those rules; the contract describes them.
- **Proto types stay at the edge.** Handlers convert between proto messages and the service's own types. The domain and the store never import generated code, so the wire format can change without touching business logic.
- **Money is never a float.** An amount is an `int64` in minor units plus an ISO 4217 currency code, as in `pkg/money` ([ADR-0004](../docs/adr/0004-represent-money-as-int64-minor-units.md)). Never use `float` or `double` for money.
- **Bump the tools on purpose.** buf's version is in CI (`go install …buf@vX`); keep it in step with your local `buf --version`. protoc-gen-go is a Go tool in `go.mod`: change it with `go get -tool google.golang.org/protobuf/cmd/protoc-gen-go@vX`, then run `make proto` and commit the regenerated code.
