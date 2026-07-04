# Pricing Overrides

Detent uses the embedded default pricing table when `budget.pricing_path` is empty or set to `priv/pricing/models.yaml`. To override model pricing, point `budget.pricing_path` at a YAML file with a `models` map.

Each model row must include input, cached-input, and output rates. Use either per-token keys:

```yaml
models:
  gpt-example:
    usd_per_input_token: 0.000005
    usd_per_cached_input_token: 0.0000005
    usd_per_output_token: 0.000030
```

or per-million-token keys:

```yaml
models:
  gpt-example:
    input_usd_per_1m_tokens: 5.00
    cached_input_usd_per_1m_tokens: 0.50
    output_usd_per_1m_tokens: 30.00
```

`cached_input_usd_per_1m_tokens` and `usd_per_cached_input_token` are cache-read prices, not cache-write prices. Verify all three columns against the provider's published pricing when adding or changing a model. If an override row omits the cached-input rate, Detent logs a warning and falls back to the input rate so the model does not silently price cache reads at zero.
