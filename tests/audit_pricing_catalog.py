"""Read-only audit of a models.dev catalog; accepts its JSON path."""
import collections
import json
import sys

catalog = json.load(open(sys.argv[1], encoding="utf-8"))
groups = collections.defaultdict(list)
stats = collections.Counter()
examples = collections.defaultdict(list)
for provider, data in sorted(catalog.items()):
    for model_id, model in sorted(data.get("models", {}).items()):
        stats["models"] += 1
        cost = model.get("cost")
        if not isinstance(cost, dict):
            stats["without_cost"] += 1
            continue
        stats["priced_entries"] += 1
        key = model_id.lower().split("/")[-1]
        groups[key].append((provider, model_id, cost, model.get("canonical_model_id")))
        for issue, condition in {
            "missing_input_or_output": not all(k in cost for k in ("input", "output")),
            "all_zero": all(cost.get(k, 0) == 0 for k in ("input", "output", "cache_read", "cache_write")),
            "tiers": bool(cost.get("tiers")),
            "legacy_context": bool(cost.get("context_over_200k")),
            "negative_rates": any(isinstance(v, (int, float)) and v < 0 for v in cost.values()),
        }.items():
            if condition:
                stats[issue] += 1
                if len(examples[issue]) < 8:
                    examples[issue].append({"provider": provider, "id": model_id, "cost": cost})
        canonical = model.get("canonical_model_id")
        if canonical:
            stats["canonical_id"] += 1
            if canonical.split("/")[0] == provider:
                stats["canonical_provider_matches"] += 1

collisions = []
for key, rows in groups.items():
    rates = {tuple(row[2].get(k) for k in ("input", "output", "cache_read", "cache_write")) for row in rows}
    per_channel = collections.defaultdict(set)
    for provider, _, cost, _ in rows:
        per_channel[provider].add(tuple(cost.get(k) for k in ("input", "output", "cache_read", "cache_write")))
    stats["within_provider_suffix_conflicts"] += sum(len(rates) > 1 for rates in per_channel.values())
    if len(rows) > 1:
        stats["duplicate_bare_ids"] += 1
    if len(rates) > 1:
        stats["conflicting_bare_ids"] += 1
        collisions.append({"id": key, "providers": len(rows), "rates": len(rates)})
print(json.dumps({"providers": len(catalog), "stats": stats, "examples": examples,
                  "largest_collisions": sorted(collisions, key=lambda x: -x["providers"])[:20]}, indent=2))
