# Architecture Decision Records

Why a shipped choice looks the way it does. Agents write these during the change. The procedure is the `decision-record` skill.

| Status | Meaning |
| --- | --- |
| `accepted` | Current choice. Matching behavior is intended. |
| `proposed` | Not yet confirmed. Matching behavior is not protected. |
| `superseded` | Replaced. The old file stays, and it links to the new ADR. |

## Index

| ID | Status | Title |
| --- | --- | --- |
| [ADR-0001](0001-guest-allocation-failures.md) | accepted | Guest allocation failures stay Python errors |
