# Zirox SNMP Exporter

SNMP-экспортер для Prometheus с веб-интерфейсом. Устройства добавляются в браузере. При добавлении устройство опрашивается **один раз целиком**: находятся порты, CPU, память, датчики температуры, БП и вентиляторы. Дальше по расписанию опрашиваются **только отмеченные галочками** объекты, точечными SNMP GET без walk. Prometheus забирает значения из памяти экспортера и к устройствам не обращается.

## Как это работает

1. **Добавление / Refresh.** Полный опрос: `ifTable`/`ifXTable`, CISCO-PROCESS-MIB, CISCO-MEMORY-POOL-MIB, CISCO-SYSTEM-EXT-MIB (Nexus), ENTITY-SENSOR-MIB, CISCO-ENVMON-MIB, HOST-RESOURCES-MIB, UCD-SNMP-MIB.
2. **Выбор.** В карточке устройства все объекты показаны списком с галочками. По умолчанию отмечены физические порты в состоянии up и все объекты здоровья. Порты в состоянии down и VLAN-интерфейсы не отмечены.
3. **Опрос.** Только отмеченные объекты, пачками до 30 OID в одном GET. Для 48-портового коммутатора с 10 отмеченными портами это 4 небольших запроса за интервал.
4. **Refresh.** Повторный полный опрос. Новые порты подсвечиваются и, если они up, сразу отмечаются (это настраивается). Пропавшие помечаются и больше не опрашиваются. Ваши галочки сохраняются, порты сопоставляются по имени (`ifName`), а не по `ifIndex`.

### Частота опроса

Интервал задаётся в секундах на трёх уровнях, более конкретный побеждает:

| Уровень | Где задаётся |
| --- | --- |
| Устройство | Карточка устройства → «Изменить» → «Свой интервал» |
| Профиль опроса | Poll profiles (по умолчанию: Fast 6 s, Normal 60 s, Slow 300 s) |
| Глобальный | «Настройки» (по умолчанию 60 с) |

Изменения применяются кнопкой сразу, без перезапуска. Минимум 5 с. Если устройство не отвечает, интервал для него временно увеличивается (2×, 4×… до 10 минут), чтобы не долбить недоступный хост. Не более 64 одновременных опросов и 8 одновременных полных опросов (`ZIROX_MAX_POLLS`, `ZIROX_MAX_DISCOVERY`).

## Запуск

### Docker

```bash
docker compose up -d --build
docker logs zirox   # если ZIROX_ADMIN_PASSWORD пуст, здесь будет сгенерированный пароль admin
```

Откройте `http://<сервер>:9116`.

### Без Docker (бинарник + systemd)

```bash
CGO_ENABLED=0 go build -o zirox ./cmd/zirox      # нужен Go 1.26+
sudo install -m 755 zirox /usr/local/bin/zirox
sudo useradd --system --no-create-home zirox
sudo mkdir -p /etc/zirox && echo 'ZIROX_ADMIN_PASSWORD=смените-меня' | sudo tee /etc/zirox/zirox.env
sudo cp deploy/zirox.service /etc/systemd/system/ && sudo systemctl enable --now zirox
```

### Переменные окружения

| Переменная | По умолчанию | Назначение |
| --- | --- | --- |
| `ZIROX_LISTEN` | `:9116` | Адрес HTTP-сервера |
| `ZIROX_DATA_DIR` | `./data` | База SQLite и ключ шифрования |
| `ZIROX_ADMIN_USER` | `admin` | Логин администратора (создаётся при первом запуске) |
| `ZIROX_ADMIN_PASSWORD` | случайный | Пароль администратора при первом запуске |
| `ZIROX_RESET_ADMIN_PASSWORD` | — | `1` = сбросить пароль на `ZIROX_ADMIN_PASSWORD` при старте |
| `ZIROX_SECRET_KEY` | файл `data/secret.key` | 32 байта в base64 для шифрования community и паролей v3 |
| `ZIROX_SCRAPE_TOKEN` | — | Требовать `Authorization: Bearer <token>` на `/probe`, `/sd`, `/metrics` |
| `ZIROX_SD_ADDRESS` | Host из запроса | `host:port`, по которому Prometheus ходит к экспортеру |
| `ZIROX_COOKIE_SECURE` | — | `1`, если UI открыт через HTTPS |
| `ZIROX_TLS_CERT`, `ZIROX_TLS_KEY` | — | Включить HTTPS прямо в экспортере |
| `ZIROX_MAX_POLLS` | `64` | Одновременных опросов |
| `ZIROX_MAX_DISCOVERY` | `8` | Одновременных полных опросов |

**Делайте резервную копию `data/secret.key`** вместе с базой: без ключа сохранённые community и пароли не расшифровать.

## Prometheus

```yaml
scrape_configs:
  - job_name: zirox-snmp
    http_sd_configs:
      - url: http://zirox-host:9116/sd
        refresh_interval: 60s
```

Через HTTP Service Discovery Prometheus получает список устройств и интервал каждого (`__scrape_interval__`). Новое устройство из веб-интерфейса появляется в Prometheus без правки конфига. Каждое устройство — отдельная цель с метками `instance`, `device`, `group`, `address`, `sysName`.

| Эндпоинт | Что отдаёт |
| --- | --- |
| `/sd` | Список целей для HTTP SD (JSON) |
| `/probe?target=<id\|имя\|IP>` | Метрики одного устройства из кэша |
| `/metrics/all` | Все устройства одним ответом, с метками `device`, `group` |
| `/metrics` | Состояние самого экспортера |
| `/healthz` | Проверка живости |

### Метрики

Порты (имена как у стандартного snmp_exporter, готовые дашборды Grafana подходят): `ifHCInOctets`, `ifHCOutOctets`, `ifHCInUcastPkts`, `ifHCOutUcastPkts`, `ifInErrors`, `ifOutErrors`, `ifInDiscards`, `ifOutDiscards`, `ifOperStatus`, `ifAdminStatus`, `ifHighSpeed` с метками `ifIndex`, `ifName`, `ifDescr`, `ifAlias`. Для SNMPv1 вместо 64-битных счётчиков отдаются `ifInOctets`/`ifOutOctets`/`ifSpeed`.

Здоровье устройства: `zirox_cpu_utilization_percent{window}`, `zirox_memory_used_bytes`, `zirox_memory_free_bytes`, `zirox_memory_utilization_percent`, `zirox_memory_total_bytes`, `zirox_memory_available_bytes`, `zirox_temperature_celsius`, `zirox_sensor_value{unit}`, `zirox_sensor_status`, `zirox_envmon_{temperature,fan,psu}_state`, `zirox_storage_{size,used}_bytes`.

Служебные: `zirox_poll_success`, `zirox_poll_duration_seconds`, `zirox_poll_last_success_timestamp_seconds`, `zirox_poll_interval_seconds`, `zirox_poll_samples`, `zirox_device_info`.

Примеры запросов:

```promql
rate(ifHCInOctets{device="core-nx-01"}[5m]) * 8   # входящий трафик, бит/с
increase(ifInErrors[1h]) > 0                        # порты с ошибками за час
zirox_poll_success == 0                             # устройства, которые не отвечают
```

### Алерты

`deploy/alerts.yml` — готовые правила для Prometheus: устройство не отвечает 5 минут, CPU > 90 %, память > 90 %, температура > 70 °C, проблема с БП/вентилятором/датчиком (CISCO-ENVMON), диск заполнен > 90 %, порт упал при admin up, ошибки на порту, загрузка порта > 90 %. Пороги правьте под себя.

```yaml
rule_files:
  - /etc/prometheus/zirox-alerts.yml
```

Проверка: `promtool check rules deploy/alerts.yml && promtool test rules deploy/alerts_test.yml`.

## Grafana

`deploy/grafana-dashboard.json` — дашборд «Zirox SNMP». Импорт: Grafana → Dashboards → New → Import → загрузить файл, выбрать источник Prometheus. Фильтры сверху: группа, устройство, интерфейс. Разделы:

- **Overview** — сколько устройств, сколько не отвечает, упавшие порты, аппаратные проблемы, таблица всех устройств со временем последнего успешного опроса;
- **Health** — CPU, память, температура, диски, состояние БП и вентиляторов;
- **Interfaces** — входящий/исходящий трафик, загрузка в %, ошибки и discards, таблица портов со статусом и скоростью.

Так как имена метрик портов совпадают со стандартным snmp_exporter, подходят и готовые дашборды из каталога Grafana.

## Импорт из CSV

«Импорт» принимает CSV с заголовком. Обязательна только колонка `address`. Остальные необязательные: `name`, `port`, `group`, `profile` (имя профиля), `credential` (имя сохранённых учётных данных) или `version` + `community` для v1/v2c.

```csv
name,address,group,profile,credential
core-nx-01,10.0.0.1,DC1,Fast (6 s),Nexus RO v3
acc-sw-12,10.0.1.12,DC1,,
```

## Безопасность

- Вход в UI по логину и паролю (bcrypt), сессия в подписанной cookie (`HttpOnly`, `SameSite=Strict`), POST-запросы с чужого сайта отклоняются.
- Community и пароли SNMPv3 хранятся в базе в зашифрованном виде (AES-256-GCM) и никогда не показываются в интерфейсе.
- Поддерживается SNMPv3 authPriv (SHA/SHA-2, AES/AES-192/AES-256, DES).

## Разработка

```bash
go test ./...
# Интеграционный тест с настоящим snmpd (v1, v2c, v3), см. .github/workflows/ci.yml:
ZIROX_TEST_SNMP=127.0.0.1:1161 go test ./internal/snmpx/
```

Структура: `cmd/zirox` — точка входа; `internal/snmpx` — SNMP, discovery, опрос, описание OID; `internal/engine` — расписание и кэш; `internal/exporter` — `/probe`, `/sd`, `/metrics`; `internal/web` — веб-интерфейс; `internal/store` — SQLite; `internal/secret` — шифрование.
