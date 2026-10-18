| Метрика | Baseline (vector-RAG, топ-1) | Агентний pipeline |
|---|---|---|
| top1_relevance / recall (mean, 20 запитів) | 0.60 | 0.88 |
| groundedness (mean) | 1.00 | 1.00 |
| latency_p95, ms | 1.67 | 4.88 |
| tokens_used (проксі, mean слів у відповіді) | 16.0 | 80.9 |

| # | Запит | Тип | rel (base) | rel (agentic) |
|---|---|---|---|---|
| 1 | tariff-t2-def | single-hop | 1.00 | 1.00 |
| 2 | tariff-t1-def | single-hop | 0.00 | 1.00 |
| 3 | rate-t2 | single-hop | 1.00 | 1.00 |
| 4 | rate-t1 | single-hop | 1.00 | 1.00 |
| 5 | signer-014 | single-hop | 1.00 | 1.00 |
| 6 | signer-031 | single-hop | 1.00 | 1.00 |
| 7 | signer-007 | single-hop | 1.00 | 1.00 |
| 8 | finding-acme | single-hop | 1.00 | 1.00 |
| 9 | availability-northwind | single-hop | 1.00 | 1.00 |
| 10 | nbu-deadline | single-hop | 1.00 | 1.00 |
| 11 | canonical-t2-signers | multi-hop | 0.00 | 1.00 |
| 12 | a114-rate-and-signer | multi-hop | 0.50 | 1.00 |
| 13 | a207-tariff-and-signer | multi-hop | 0.50 | 1.00 |
| 14 | a331-signer-and-nbu | multi-hop | 0.00 | 1.00 |
| 15 | northwind-service-and-incidents | multi-hop | 0.50 | 0.50 |
| 16 | f101-control-and-auditor | multi-hop | 0.00 | 0.50 |
| 17 | nbu-requirement-and-deadline | multi-hop | 0.50 | 0.50 |
| 18 | globex-auditor-and-finding | multi-hop | 0.00 | 0.50 |
| 19 | t1-rate-and-signer | multi-hop | 0.50 | 1.00 |
| 20 | acme-findings-and-severity | multi-hop | 0.50 | 0.50 |
