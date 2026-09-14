# C3 screening reason codes

Stable, documented strings reported to C1 as the `reason` on a screening
transition (see §A of `docs/03-build/c3-screening-build-prompts.md`). One
code per condition, never reused for a different condition -- same
discipline C1 applies to its own error codes.

Defined in `internal/verdict/verdict.go`.

| Code | Meaning | Produced by |
|---|---|---|
| `screening_pass` | Provider returned a clean result within the timeout, below both thresholds, not vendor-flagged. | `verdict.Classify` |
| `screening_hold_flagged` | The provider explicitly flagged the address (`Verdict.Flagged == true`), **or** its risk score is at or above `Thresholds.FlaggedAtOrAbove` even without an explicit vendor flag. An explicit `Flagged=true` always wins regardless of score. | `verdict.Classify` |
| `screening_hold_ambiguous` | Risk score falls in the configured grey band: at or above `Thresholds.PassBelow` but below `Thresholds.FlaggedAtOrAbove`, and the vendor did not explicitly flag it. | `verdict.Classify` |
| `screening_hold_unavailable` | Every `provider.ScreenWithPolicy` retry was exhausted and `OutagePolicy` is `FailClosed` (the default). There is no real `Verdict` to classify and no backing `screening_results` row. | `verdict.Unavailable` |
| `screening_pass_vendor_unavailable` | Every `provider.ScreenWithPolicy` retry was exhausted and `OutagePolicy` is `FailOpen` -- the order is passed through anyway, but never as a plain `screening_pass`, so the vendor-outage origin stays auditable. No backing `screening_results` row, same as the `FailClosed` case above. | `verdict.PassVendorUnavailable` |
| `screening_hold_stale_cache_invalidated` | An operator invalidated a prior cached pass (`internal/cache.Invalidate`) and the order is re-held pending a fresh screen. | `internal/cache.Invalidate` + `verdict.ReasonHoldStaleCacheInvalidated` (C3.7, built) |

## Threshold boundary semantics

`Thresholds.PassBelow` and `Thresholds.FlaggedAtOrAbove` are both
inclusive on the hold side: a score *exactly equal* to either cutoff is
treated as "far enough into the band to hold," never "not quite there
yet." Concretely, for a non-flagged verdict:

```
score <  PassBelow            -> Pass
PassBelow <= score < FlaggedAtOrAbove -> Hold, screening_hold_ambiguous
score >= FlaggedAtOrAbove     -> Hold, screening_hold_flagged
```

`DefaultThresholds` (`PassBelow: 0.5`, `FlaggedAtOrAbove: 0.85`) is a
placeholder, not a calibrated policy -- no screening vendor is chosen yet
(§B of the build spec), so nobody has seen a real score distribution to
tune these against. Revisit once one is.

## Outage policy (C3.5)

`provider.ScreenWithPolicy(ctx, provider, address, timeout, retries, policy)`
retries a vendor call up to `retries` times (exponential backoff between
attempts, capped at `provider.DefaultBackoffMax`), each attempt bounded
by `timeout`. The first successful attempt wins outright -- its real
`Verdict` is classified normally, never touched by outage handling.

Once every attempt is exhausted, `OutagePolicy` decides what happens,
and this decision is deliberately never routed through
`verdict.Classify`: the synthetic placeholder `ScreenWithPolicy` returns
(tagged via `provider.ReasonProviderUnavailable` in its `ReasonCodes`,
checked with `provider.IsProviderUnavailable`) is not a real vendor
response, and `Classify` has no way to tell it apart from a genuinely
clean `RiskScore: 0`. Instead:

- **`FailClosed`** (`provider.OutagePolicy` zero value, the shipped
  default): `ScreenWithPolicy` returns an error wrapping
  `provider.ErrProviderUnavailable`; the caller reports
  `verdict.Unavailable()` (`screening_hold_unavailable`).
- **`FailOpen`** (opt-in only, config + a restart -- never a runtime
  toggle): `ScreenWithPolicy` returns the tagged placeholder alongside a
  `nil` error; the caller reports `verdict.PassVendorUnavailable()`
  (`screening_pass_vendor_unavailable`).

Neither outcome writes a `screening_results` row -- there was no real
vendor response to cache. Both increment a
`screening_vendor_unavailable_total`-style metric (`internal/pipeline`'s
own `MetricsRecorder.VendorUnavailable()`), regardless of policy, so an
operator can see vendor degradation even when `FailOpen` is masking it
from the order pipeline's own behavior.
