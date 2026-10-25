# ДЗ 8

## Запуск

```bash
cd week4/Day8_Fault_Tolerant_Tool_Harness/labs8
go build ./...
go test -race ./...

LABS8_MODEL=demo go run . "Open a refund for txn-2026-07-118845 at merchant A-114"   # офлайн, без інфри

# для живого прогону з Temporal + Dgraph:
temporal server start-dev --db-filename .data/temporal.db
docker compose up -d dgraph
LABS8_MODEL=demo go run . worker
LABS8_MODEL=demo go run . start -session s1 "Open a refund for txn-2026-07-118845 at merchant A-114"
go run . counter
```

Воркер і `start` — завжди з однаковим префіксом (`LABS8_MODEL=demo` в обох або справжній ключ в обох).

## Що зробив

- Агент сам розрізняє три типи збоїв: тимчасовий (мережа) — просто повторює; сам винен (криві аргументи) — показує собі помилку і виправляється, з лімітом на кількість спроб; фатальний — здається.
- Цикл агента переробив на Temporal Workflow: кожен виклик моделі чи інструмента — окрема Activity. Перевірив по-справжньому: вбив процес `kill -9` посеред роботи, після перезапуску хід довершився, а вже зроблені LLM-виклики не повторились.
- Сесія розмови зберігається в SQLite — другий запуск з тим самим `-session` продовжує, а не починає з нуля.
- Пам'ять зберігається в Dgraph — перевірив: повний рестарт усіх сервісів, нова сесія, і агент все ще пам'ятає факти зі старої.
- Три офлайн-тести на типові збої (таймаут, криві дані, недоступний сервіс) — без Docker і без живого Temporal.
