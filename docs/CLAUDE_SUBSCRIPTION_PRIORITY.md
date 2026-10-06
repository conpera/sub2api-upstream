# Claude subscription pool priority

For opted-in request groups whose resolved target platform is Anthropic,
scheduling first tries Anthropic OAuth and Setup Token accounts. API keys and eligible mixed-platform
accounts form the fallback pool. Account names, numerical priorities, and model
names do not determine pool membership.

The policy defaults to disabled. Enable it for explicit group IDs using the
admin settings API below; no account edits or schema migration are needed.
Other groups and target platforms retain their existing scheduler. Shared
accounts do not transfer the policy to their other groups. After Claude Code
fallback, selection uses the destination group's policy.
Existing account priority settings cannot enforce this across model routes and
sticky bindings. A frontend or external proxy cannot atomically acquire the
scheduler's account slots; the exception is therefore scoped to selection inside
the existing gateway, with its eligibility and billing lifecycle preserved.

## Runtime configuration and display

`GET /api/v1/admin/settings/claude-subscription-priority` returns the runtime
settings shared with the scheduler. The standard response envelope contains:

```json
{
  "supported": true,
  "strategy": "subscription_first",
  "enabled_group_ids": [],
  "subscription_account_types": ["oauth", "setup-token"]
}
```

`PUT` on the same admin-only endpoint requires an explicit `enabled_group_ids`
array. Empty `[]` disables the policy everywhere. Positive IDs are deduplicated,
sorted and validated against active Anthropic/Composite groups. Only the
`claude_subscription_priority_group_ids` settings key is updated. Successful
writes update the local immutable cache immediately; other processes observe
changes within its five-second TTL. An expired read failure disables the new
selection behavior and is returned as an error by the status endpoint.

The initial requested production scope is Fable group 28, not an account list or
Channel entity. The monitor at `/account-monitor/claude` reads this endpoint and
maps the enabled IDs to group names and per-account pool labels. Missing API
support or a failed status read must display unconfirmed status, never enabled.
The monitor implementation and deployment belong to `sub2api-oauth-checker`.

## Selection order

1. Apply the existing group, platform, model, account state, cooldown, quota,
   privacy, window cost, RPM, session limit, and profit admission rules.
2. Try the subscription pool. Within that pool retain model routing, sticky
   session preference, and ordinary priority/load ordering.
3. If no eligible subscription can acquire a slot, try the regular pool using
   the same within-pool preferences. A binding or model route in the regular
   pool cannot bypass an available subscription.
4. Only after immediate attempts in both pools fail, choose a wait plan,
   preferring the subscription pool. Do not register speculative waiting
   sessions and then abandon them when the other pool succeeds.

With subscriptions present, cached/effective load is an ordering hint rather
than proof that an account is full. Every eligible subscription can be tried;
there is no Top-K cutoff. Real concurrency acquisition decides whether a slot
is available. Acquisition errors stop selection instead of being treated as
capacity exhaustion. Disabled/failed batch load reads retain pool ordering.

When no subscription accounts are present, retain the original ordinary
scheduler, including its sticky waiting and load filtering. Requests already
forwarded to a regular account continue there. Individual acquisitions are
atomic, but the multi-account scan is not one cross-account transaction: a
subscription can release a slot after it was checked.

This is pool priority, not strict numerical priority across all requests.
Within a pool, existing routing and sticky preferences may precede priority.
Existing profit-controlled binding preservation is unchanged; each new
selection still checks the subscription pool first.

`SelectAccountForModelWithExclusions` also respects pool order for token-count
and other selection-only callers, without acquiring a generation slot or
registering a session. Composite aliases retain their upstream model and
account-ownership context, including when load batching is disabled. Credential
hydration failures release a newly acquired slot before returning an error.

The event `claude.subscription_pool_selected` records the pool, account ID,
group ID and whether a slot was acquired; `acquired=false` is a wait plan, not
proof of an upstream request. No credentials or request content are logged.

## Validation

Use Go 1.27.0 and the `unit` build tag. Regression tests use local synthetic
accounts and mocked concurrency/session caches, with no production access:

```sh
cd backend
go test -tags=unit ./internal/service -run '^TestClaudeSubscriptionPriority_' -count=1
go test -race -tags=unit ./internal/service -run '^TestClaudeSubscriptionPriority_' -count=1
go test -tags=unit ./internal/service
go test -tags=unit ./internal/handler -run 'Test.*(Gateway|Profit|Concurrency|Failover)' -count=1
```

The release candidate is based on production source
`e36dd4c6d30d548e79f1e02430401c40d1891e87`. Test and build receipts, production
activation status, and the exact candidate artifact belong in the task's
release report; this document alone is not deployment evidence.

## Release and rollback boundary

Build with the matching frontend assets and `-tags embed`. The existing release
mechanism replaces the running binary and restarts Sub2; active requests can be
interrupted. Deploy only after approval for that restart, using fresh runtime
identity checks and the established release tooling rather than old Compose
definitions.

Before activation retain the actual running binary. The current deployment
uses a replaced binary inside an older image, so the image label is not a valid
rollback baseline. Roll back the binary while retaining the current database,
Redis state and consumption ledger; never restore an old ledger for this change.
